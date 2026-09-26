package httpapi

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// binResponse is the paginated Bin listing.
type binResponse struct {
	Artifacts  []artifactResponse `json:"artifacts"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// handleCreate streams a body (raw) or multipart form (single file, or a bundle
// for multiple files) into a new artifact. Provenance channel is server-derived
// from the authenticated principal, never a client claim.
//
// Governing: SPEC-0002 REQ "Artifact Lifecycle — Create", REQ "Bundles with N
// Members", REQ "Request Body Size Limits".

// requestModel reads the optional model an API client reports for the artifact
// it is creating, from ?model= or the X-Cairn-Model header — the same
// query-or-header pairing `title` already uses, so a caller that can set one
// can set the other. Empty is normal and means "not reported": a human piping a
// file from the CLI has no model to name, and the viewer omits the row.
func requestModel(r *http.Request) string {
	return firstNonEmpty(r.URL.Query().Get("model"), r.Header.Get("X-Cairn-Model"))
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		s.createMultipart(w, r, p, params["boundary"])
		return
	}
	s.createSingle(w, r, p)
}

// requestedTTL parses the optional X-Cairn-Ttl-Seconds header (SPEC-0008
// `cairn --ttl`): a positive integer number of seconds bounded by
// cfg.MaxRequestedTTL. An absent header is not an error — the caller falls
// back to cfg.DefaultTTL — but a present, malformed, non-positive, or
// over-the-cap value IS validation_failed: the CLI never silently gets a
// different TTL than what it explicitly asked for (see Config.MaxRequestedTTL
// docs). The server remains authoritative (ADR-0007): this only interprets an
// explicit ask, it never computes one.
func requestedTTL(r *http.Request, cfg Config) (time.Duration, error) {
	raw := r.Header.Get(ttlHeader)
	if raw == "" {
		return cfg.DefaultTTL, nil
	}
	ttl, inv := checkTTLSeconds(ttlHeader, errs.LocHeader, raw, cfg.MaxRequestedTTL)
	if inv != nil {
		return 0, inv
	}
	return ttl, nil
}

// ttlHeader is the create path's requested-TTL header.
const ttlHeader = "X-Cairn-Ttl-Seconds"

// ttlExpect is how a valid TTL is described in a violation's message.
const ttlExpect = "a positive integer number of seconds"

// checkTTLSeconds validates a requested TTL, given as decimal seconds, against
// the server's cap. The create header and the policy body share it, so both
// TTL surfaces agree on units, bounds and the violation they report.
//
// Governing: ADR-0025, SPEC-0019 VE-1 (scenarios "TTL over the cap",
// "Malformed TTL"), ADR-0007
func checkTTLSeconds(field string, loc errs.Location, raw string, max time.Duration) (time.Duration, *errs.Invalid) {
	secs, err := strconv.ParseInt(raw, 10, 64)
	switch {
	case err != nil:
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
			return 0, ttlTooLong(field, loc, raw, max)
		}
		return 0, errs.Violate(field, loc, errs.ReasonInvalidFormat, errs.WithValue(raw), errs.WithExpect(ttlExpect))
	case secs <= 0:
		return 0, errs.Violate(field, loc, errs.ReasonNotPositive, errs.WithValue(raw), errs.WithExpect(ttlExpect))
	case secs > int64(max/time.Second):
		return 0, ttlTooLong(field, loc, raw, max)
	}
	return time.Duration(secs) * time.Second, nil
}

func ttlTooLong(field string, loc errs.Location, raw string, max time.Duration) *errs.Invalid {
	return errs.Violate(field, loc, errs.ReasonExceedsMax,
		errs.WithValue(raw), errs.WithLimit(int64(max/time.Second), errs.UnitSeconds))
}

func (s *Server) createSingle(w http.ResponseWriter, r *http.Request, p *Principal) {
	shareType := artifact.ShareType(firstNonEmpty(
		r.URL.Query().Get("type"), r.Header.Get("X-Cairn-Type"), string(artifact.TypeFile)))
	now := s.now()

	// The TTL and every tag are checked together, so one rejection names all
	// of them (SPEC-0019 VE-4).
	var tags tagSet
	ttl, ttlErr := requestedTTL(r, s.cfg)
	downgrade, redErr := redactionDowngrade(redactionHeader, errs.LocHeader, r.Header.Get(redactionHeader))
	if err := errs.JoinErrors(ttlErr, redErr, tags.addFromRequest(r)); err != nil {
		s.writeError(w, r, err, nil)
		return
	}

	// Guard the raw body; the store additionally enforces the limit incrementally.
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1)
	art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
		ShareType:          shareType,
		Title:              firstNonEmpty(r.URL.Query().Get("title"), r.Header.Get("X-Cairn-Title")),
		Body:               body,
		DeclaredMediaType:  r.Header.Get("Content-Type"),
		ExpectedSHA256:     r.Header.Get("X-Cairn-Sha256"),
		Provenance:         artifact.Provenance{ActorID: p.ActorID, Model: requestModel(r), Channel: p.Channel, CapturedAt: now},
		Access:             artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink},
		ExpiresAt:          now.Add(ttl),
		Tags:               tags.tags,
		RedactionDowngrade: downgrade,
	})
	if err != nil {
		s.writeError(w, r, s.mapUploadErr(err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toOwnerArtifactResponse(art))
}

// spooledFile is a multipart file part spooled to a temp file so the store can
// stream each member independently (multipart parts are strictly sequential).
type spooledFile struct {
	name  string
	media string
	file  *os.File
	size  int64
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, p *Principal, boundary string) {
	if boundary == "" {
		s.writeError(w, r, errs.Validationf("multipart: missing boundary"), nil)
		return
	}
	// The TTL, header and query tags are checked with the form's own tag
	// fields, so one rejection names every bad tag from every source (SPEC-0019
	// VE-4). They are reported at the first file part, before any body is
	// spooled.
	var tags tagSet
	ttl, ttlErr := requestedTTL(r, s.cfg)
	downgrade, redErr := redactionDowngrade(redactionHeader, errs.LocHeader, r.Header.Get(redactionHeader))
	_ = tags.addFromRequest(r)
	mr := multipart.NewReader(r.Body, boundary)

	var (
		title string
		files []spooledFile
	)
	cleanup := func() {
		for _, f := range files {
			_ = f.file.Close()
			_ = os.Remove(f.file.Name())
		}
	}
	defer cleanup()

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.writeError(w, r, errs.Validationf("multipart: malformed body"), nil)
			return
		}
		if part.FileName() == "" {
			switch part.FormName() {
			case "title":
				b, _ := io.ReadAll(io.LimitReader(part, 4096))
				title = string(b)
			case "tag":
				tags.addFormField(part)
			}
			_ = part.Close()
			continue
		}
		if err := errs.JoinErrors(ttlErr, redErr, tags.err()); err != nil {
			_ = part.Close()
			s.writeError(w, r, err, nil)
			return
		}
		tmp, err := os.CreateTemp("", "cairn-upload-*")
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		// Copy at most MaxUploadBytes+1 so an oversize part is detected precisely.
		n, copyErr := io.Copy(tmp, io.LimitReader(part, s.cfg.MaxUploadBytes+1))
		_ = part.Close()
		if copyErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			s.writeError(w, r, copyErr, nil)
			return
		}
		if n > s.cfg.MaxUploadBytes {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			s.writeError(w, r, s.mapUploadErr(errs.ErrTooLarge), nil)
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			s.writeError(w, r, err, nil)
			return
		}
		files = append(files, spooledFile{name: part.FileName(), media: part.Header.Get("Content-Type"), file: tmp, size: n})
	}

	if err := errs.JoinErrors(ttlErr, redErr, tags.err()); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	if len(files) == 0 {
		s.writeError(w, r, errs.Validationf("multipart: no file parts"), nil)
		return
	}

	now := s.now()
	prov := artifact.Provenance{ActorID: p.ActorID, Model: requestModel(r), Channel: p.Channel, CapturedAt: now}
	access := artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink}
	expires := now.Add(ttl)

	if len(files) == 1 {
		f := files[0]
		art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
			ShareType:          artifact.ShareType(firstNonEmpty(r.URL.Query().Get("type"), string(artifact.TypeFile))),
			Title:              firstNonEmpty(title, f.name),
			Body:               f.file,
			DeclaredMediaType:  f.media,
			Provenance:         prov,
			Access:             access,
			ExpiresAt:          expires,
			Tags:               tags.tags,
			RedactionDowngrade: downgrade,
		})
		if err != nil {
			s.writeError(w, r, s.mapUploadErr(err), nil)
			return
		}
		s.writeJSON(w, http.StatusCreated, s.toOwnerArtifactResponse(art))
		return
	}

	members := make([]store.MemberInput, 0, len(files))
	for _, f := range files {
		members = append(members, store.MemberInput{Name: f.name, Body: f.file, DeclaredMediaType: f.media})
	}
	art, err := s.store.CreateBundle(r.Context(), store.CreateBundleInput{
		Title:              title,
		Members:            members,
		Provenance:         prov,
		Access:             access,
		ExpiresAt:          expires,
		Tags:               tags.tags,
		RedactionDowngrade: downgrade,
	})
	if err != nil {
		s.writeError(w, r, s.mapUploadErr(err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toOwnerArtifactResponse(art))
}

// handleGet resolves an artifact's metadata + preview info. Unknown/expired ids
// return a uniform 404 (SPEC-0002 REQ "Artifact Lifecycle — Read").
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	art, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	// A link read needs no credential, but an owner who sends one also sees
	// the scan outcome (SPEC-0017 RD-9).
	var viewer string
	if p, ok := s.optionalPrincipal(r); ok {
		viewer = p.ActorID
	}
	s.writeJSON(w, http.StatusOK, s.toViewerArtifactResponse(art, viewer))
}

// handleGetBody streams the raw body, re-verifiable against the stored SHA-256.
func (s *Server) handleGetBody(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	rc, info, err := s.store.OpenBody(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	defer rc.Close()
	s.serveBody(w, r, rc, info, id)
}

// handleGetMember streams a bundle member addressed as <id>/<name>.
func (s *Server) handleGetMember(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "*")
	rc, info, err := s.store.OpenMember(r.Context(), id, name)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id, "name": name})
		return
	}
	defer rc.Close()
	s.serveBody(w, r, rc, info, path.Base(name))
}

// serveBody streams bytes with a download disposition and a non-sniffable type
// so untrusted content cannot execute in Cairn's origin (SPEC-0002 REQ
// "Security Headers" — untrusted body cannot execute).
func (s *Server) serveBody(w http.ResponseWriter, r *http.Request, rc io.Reader, info store.BodyInfo, filename string) {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("X-Cairn-Checksum", info.SHA256)
	if info.SHA256 != "" {
		h.Set("ETag", strconv.Quote(info.SHA256))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		s.log.WarnContext(r.Context(), "httpapi: body stream aborted", "error", err)
	}
}

// serveInlineBody streams bytes with the artifact's real Content-Type and an
// `inline` disposition — the one deliberate exception to serveBody's
// sniff-proof download floor, reserved for the image viewer's own `<img src>`
// (handleWebImage, SPEC-0003 REQ "Image Viewer", #69). It is reachable only
// through a route gated by the registry InlineViewer capability, which only
// the image type implements, so no other body can ride this path.
// X-Content-Type-Options: nosniff (set by the web group's security-header
// middleware) still applies; it is a no-op here because Content-Type already
// names the real, store-verified media type — never a client claim (an
// artifact only ever carries share type "image" once its media type already
// passed the isImage predicate at ingest, SPEC-0002 "Previewability Detection
// at Ingest").
func (s *Server) serveInlineBody(w http.ResponseWriter, r *http.Request, rc io.Reader, info store.BodyInfo, mediaType, filename string) {
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Content-Disposition", "inline; filename="+strconv.Quote(filename))
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("X-Cairn-Checksum", info.SHA256)
	if info.SHA256 != "" {
		h.Set("ETag", strconv.Quote(info.SHA256))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		s.log.WarnContext(r.Context(), "httpapi: inline body stream aborted", "error", err)
	}
}

// handleDelete removes an artifact, owner-only. Deletion is a human-only
// capability (ADR-0004 / SPEC-0004: agents receive no delete scope and cannot
// delete on the human's behalf); the route gates this via requireHuman, and this
// belt-and-suspenders guard refuses an agent principal even if that middleware
// were ever unwired, so a delete is never performed as p.ActorID for an agent.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	if p.IsAgent {
		s.writeError(w, r, errs.ErrForbidden, nil)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.store.DeleteArtifact(r.Context(), id, p.ActorID); err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBin lists the caller's Bin, keyset-paginated.
func (s *Server) handleBin(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// ?tag= narrows the Bin to artifacts carrying every given tag (SPEC-0002
	// REQ "Artifact Tags"), in the same comma-separated, repeatable form a
	// create accepts.
	var filter tagSet
	if err := filter.addQuery(r); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	page, err := s.store.ListBin(r.Context(), p.ActorID, r.URL.Query().Get("cursor"), limit, filter.tags...)
	if err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	items := make([]artifactResponse, 0, len(page.Artifacts))
	for _, a := range page.Artifacts {
		items = append(items, s.toArtifactResponse(a))
	}
	s.writeJSON(w, http.StatusOK, binResponse{Artifacts: items, NextCursor: page.NextCursor})
}

// mapUploadErr maps a body over the upload cap — an http.MaxBytesReader
// overflow, or the store's incremental limit — to a too_large violation naming
// the cap, so it renders as a 413 that says what the limit is rather than a
// generic 500 or a bare "payload too large".
//
// Governing: ADR-0025, SPEC-0019 VE-1 (scenario "Oversize body")
func (s *Server) mapUploadErr(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || (errors.Is(err, errs.ErrTooLarge) && errs.ViolationsOf(err) == nil) {
		return errs.Violate("body", errs.LocBody, errs.ReasonTooLarge,
			errs.WithLimit(s.cfg.MaxUploadBytes, errs.UnitBytes)).Because(err)
	}
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
