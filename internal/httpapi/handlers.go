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
		// An integer too large for int64 is still an integer: its sign says
		// which bound it broke, so it is exceeds_max or not_positive, never
		// invalid_format.
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
			if strings.HasPrefix(raw, "-") {
				return 0, errs.Violate(field, loc, errs.ReasonNotPositive, errs.WithValue(raw), errs.WithExpect(ttlExpect))
			}
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
	now := s.now()

	// The TTL, share type, title and every tag are checked together, so one
	// rejection names all of them (SPEC-0019 VE-4).
	var tags tagSet
	var rels relationSet
	ttl, ttlErr := requestedTTL(r, s.cfg)
	shareType, typeErr := s.requestedShareType(r)
	title, titleErr := requestedTitle(r)
	downgrade, redErr := redactionDowngrade(redactionHeader, errs.LocHeader, r.Header.Get(redactionHeader))
	if err := errs.JoinErrors(ttlErr, typeErr, titleErr, redErr, tags.addFromRequest(r), rels.addFromRequest(r)); err != nil {
		s.writeError(w, r, err, nil)
		return
	}

	// Guard the raw body; the store additionally enforces the limit incrementally.
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1)
	creator := p.EventActor()
	art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
		ShareType:          shareType,
		Title:              title,
		Body:               body,
		DeclaredMediaType:  r.Header.Get("Content-Type"),
		ExpectedSHA256:     r.Header.Get(store.ChecksumHeader),
		Provenance:         artifact.Provenance{CreatedByUserID: p.UserID, ActorID: p.ActorID, Model: requestModel(r), Channel: p.Channel, CapturedAt: now},
		Access:             artifact.AccessPolicy{OwnerUserID: p.UserID, Visibility: artifact.VisibilityLink},
		ExpiresAt:          now.Add(ttl),
		Tags:               tags.tags,
		Relations:          rels.relations,
		Trace:              requestTrace(r),
		ActorKind:          creator.Kind,
		Auth:               creator.Auth,
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
		s.writeError(w, r, errMissingBoundary(), nil)
		return
	}
	// The TTL, header and query tags are checked with the form's own tag
	// and title fields, so one rejection names every bad tag from every source
	// (SPEC-0019 VE-4). They are reported at the first file part, before any
	// body is spooled.
	var tags tagSet
	var rels relationSet
	ttl, ttlErr := requestedTTL(r, s.cfg)
	downgrade, redErr := redactionDowngrade(redactionHeader, errs.LocHeader, r.Header.Get(redactionHeader))
	_ = tags.addFromRequest(r)
	_ = rels.addFromRequest(r)
	mr := multipart.NewReader(r.Body, boundary)

	var (
		title    string
		titleErr error
		files    []spooledFile
		parts    memberChecks
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
			s.writeError(w, r, errMalformedMultipart(err), nil)
			return
		}
		if part.FileName() == "" {
			switch part.FormName() {
			case "title":
				title, titleErr = readTitleField(part)
			case "tag":
				tags.addFormField(part)
			case "relation":
				rels.addFormField(part)
			}
			_ = part.Close()
			continue
		}
		if err := errs.JoinErrors(ttlErr, titleErr, redErr, tags.err(), rels.err()); err != nil {
			_ = part.Close()
			s.writeError(w, r, err, nil)
			return
		}
		// Past a bundle's member bound nothing more is spooled; the parts
		// already seen are reported with the count (SPEC-0019 VE-4).
		if parts.n == store.MaxBundleMembers {
			_ = part.Close()
			parts.add(part.FileName(), false, s.cfg.MaxUploadBytes)
			s.writeError(w, r, parts.err(), nil)
			return
		}
		// Once a member is bad the upload is refused whatever follows, so the
		// rest are only measured for the rejection (VE-4), never spooled: a
		// doomed request must not write up to MaxBundleMembers full-size parts
		// to disk first.
		if parts.failed() {
			src := &partReader{r: io.LimitReader(part, s.cfg.MaxUploadBytes+1)}
			n, copyErr := io.Copy(io.Discard, src)
			_ = part.Close()
			if copyErr != nil {
				s.writeError(w, r, src.classify(copyErr), nil)
				return
			}
			parts.add(part.FileName(), n > s.cfg.MaxUploadBytes, s.cfg.MaxUploadBytes)
			continue
		}
		tmp, err := createSpool()
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		// Copy at most MaxUploadBytes+1 so an oversize part is detected precisely.
		src := &partReader{r: io.LimitReader(part, s.cfg.MaxUploadBytes+1)}
		n, copyErr := io.Copy(tmp, src)
		_ = part.Close()
		if copyErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			s.writeError(w, r, src.classify(copyErr), nil)
			return
		}
		// An oversize part is dropped, not spooled, and the rest are still
		// measured, so every bad member is named together (SPEC-0019 VE-4).
		tooLarge := n > s.cfg.MaxUploadBytes
		parts.add(part.FileName(), tooLarge, s.cfg.MaxUploadBytes)
		if tooLarge {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			continue
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			s.writeError(w, r, err, nil)
			return
		}
		files = append(files, spooledFile{name: part.FileName(), media: part.Header.Get("Content-Type"), file: tmp, size: n})
	}

	if err := errs.JoinErrors(ttlErr, titleErr, redErr, tags.err(), rels.err()); err != nil {
		s.writeError(w, r, err, nil)
		return
	}
	switch {
	case parts.n == 0:
		s.writeError(w, r, errNoFileParts(), nil)
		return
	case parts.n == 1 && parts.oversize:
		// A lone file is the artifact's body, so its oversize is the body's.
		s.writeError(w, r, s.mapUploadErr(errs.ErrTooLarge), nil)
		return
	case parts.n > 1:
		if err := parts.err(); err != nil {
			s.writeError(w, r, err, nil)
			return
		}
	}

	now := s.now()
	actor := p.EventActor()
	prov := artifact.Provenance{CreatedByUserID: p.UserID, ActorID: p.ActorID, Model: requestModel(r), Channel: p.Channel, CapturedAt: now}
	access := artifact.AccessPolicy{OwnerUserID: p.UserID, Visibility: artifact.VisibilityLink}
	expires := now.Add(ttl)

	if len(files) == 1 {
		f := files[0]
		shareType, err := s.multipartShareType(r)
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
			ShareType:          shareType,
			Title:              firstNonEmpty(title, f.name),
			Body:               f.file,
			DeclaredMediaType:  f.media,
			Provenance:         prov,
			Access:             access,
			ExpiresAt:          expires,
			Tags:               tags.tags,
			Relations:          rels.relations,
			Trace:              requestTrace(r),
			ActorKind:          actor.Kind,
			Auth:               actor.Auth,
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
		Relations:          rels.relations,
		Trace:              requestTrace(r),
		ActorKind:          actor.Kind,
		Auth:               actor.Auth,
		RedactionDowngrade: downgrade,
	})
	if err != nil {
		s.writeError(w, r, s.mapUploadErr(err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toOwnerArtifactResponse(art))
}

// relationView is one relation in a read response. A target the reader cannot
// open carries only its kind: no id and no title, so a read never confirms that
// an id exists or tells a reader anything about it.
//
// Governing: SPEC-0024 REQ "Relations on Read".
type relationView struct {
	Kind      artifact.RelationKind `json:"kind"`
	ID        string                `json:"id,omitempty"`
	Title     string                `json:"title,omitempty"`
	Available bool                  `json:"available"`
}

// relationViews shapes the store's relation views for the wire. The store owns
// which targets are visible to this reader; this only renames its field to the
// one the API uses for an id.
func relationViews(in []store.RelationView) []relationView {
	if len(in) == 0 {
		return nil
	}
	out := make([]relationView, 0, len(in))
	for _, r := range in {
		out = append(out, relationView{Kind: r.Kind, ID: r.PublicID, Title: r.Title, Available: r.Available})
	}
	return out
}

// memberView is one bundle member in a read response. A bundle artifact has no
// body of its own, so a reader that cannot list members has no way to find the
// name that GET /v1/artifacts/{id}/members/<name> needs — the MCP read tool
// already returns this shape (mcpMemberView); the REST read returns it too so
// the two surfaces do not diverge (ADR-0003).
type memberView struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// artifactReadResponse is GET /v1/artifacts/{id}'s payload: the artifact
// response, plus a bundle's members in bundle order, the artifact's relations as
// this reader can see them, what names it, and the trace that produced it.
//
// Relations and trace_id are omitted when empty, so a read of an artifact that
// declares nothing is byte-for-byte what it was before relations existed.
type artifactReadResponse struct {
	artifactResponse
	Members    []memberView   `json:"members,omitempty"`
	Relations  []relationView `json:"relations,omitempty"`
	Referenced []relationView `json:"referenced_by,omitempty"`
	// ReferencedByTruncated says there are more referrers than the 50 a read
	// returns (SPEC-0024 REQ "Relations on Read").
	ReferencedByTruncated bool   `json:"referenced_by_truncated,omitempty"`
	TraceID               string `json:"trace_id,omitempty"`
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
	viewer, _ := s.optionalPrincipal(r)
	var viewerID string
	if viewer != nil {
		viewerID = viewer.UserID
	}
	resp := artifactReadResponse{artifactResponse: s.toViewerArtifactResponse(art, viewerID)}
	resp.Provenance.Actor = s.displayActor(r.Context(), viewer, resp.Provenance.Actor)
	// Relations are resolved as the reader, not as the owner: the set of targets
	// one reader may see is not the set another may see (SPEC-0024 REQ "Relations
	// on Read").
	rels, err := s.store.Relations(r.Context(), store.Reader{UserID: viewerID}, id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	resp.Relations = relationViews(rels.Relations)
	resp.Referenced = relationViews(rels.ReferencedBy)
	resp.ReferencedByTruncated = rels.ReferencedByTruncated
	if art.Trace != nil {
		resp.TraceID = art.Trace.TraceHex()
	}
	if art.ShareType == artifact.TypeBundle {
		members, err := s.store.ListMembers(r.Context(), id)
		if err != nil {
			s.writeError(w, r, err, map[string]string{"id": id})
			return
		}
		resp.Members = make([]memberView, 0, len(members))
		for _, m := range members {
			resp.Members = append(resp.Members, memberView{Name: m.Name, Size: m.Size, MediaType: m.MediaType})
		}
	}
	s.writeJSON(w, http.StatusOK, resp)
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
	if err := s.store.DeleteArtifact(r.Context(), id, p.UserID); err != nil {
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
	page, err := s.store.ListBin(r.Context(), p.UserID, r.URL.Query().Get("cursor"), limit, filter.tags...)
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
