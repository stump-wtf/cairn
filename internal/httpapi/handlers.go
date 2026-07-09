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

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
	"github.com/joestump/cairn/internal/store"
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

func (s *Server) createSingle(w http.ResponseWriter, r *http.Request, p *Principal) {
	shareType := artifact.ShareType(firstNonEmpty(
		r.URL.Query().Get("type"), r.Header.Get("X-Cairn-Type"), string(artifact.TypeFile)))
	now := s.now()

	// Guard the raw body; the store additionally enforces the limit incrementally.
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1)
	art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
		ShareType:         shareType,
		Title:             firstNonEmpty(r.URL.Query().Get("title"), r.Header.Get("X-Cairn-Title")),
		Body:              body,
		DeclaredMediaType: r.Header.Get("Content-Type"),
		ExpectedSHA256:    r.Header.Get("X-Cairn-Sha256"),
		Provenance:        artifact.Provenance{ActorID: p.ActorID, Channel: p.Channel, CapturedAt: now},
		Access:            artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink},
		ExpiresAt:         now.Add(s.cfg.DefaultTTL),
	})
	if err != nil {
		s.writeError(w, r, mapUploadErr(err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toArtifactResponse(art))
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
			if part.FormName() == "title" {
				b, _ := io.ReadAll(io.LimitReader(part, 4096))
				title = string(b)
			}
			_ = part.Close()
			continue
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
			s.writeError(w, r, errs.ErrTooLarge, nil)
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

	if len(files) == 0 {
		s.writeError(w, r, errs.Validationf("multipart: no file parts"), nil)
		return
	}

	now := s.now()
	prov := artifact.Provenance{ActorID: p.ActorID, Channel: p.Channel, CapturedAt: now}
	access := artifact.AccessPolicy{OwnerID: p.ActorID, Visibility: artifact.VisibilityLink}
	expires := now.Add(s.cfg.DefaultTTL)

	if len(files) == 1 {
		f := files[0]
		art, err := s.store.CreateArtifact(r.Context(), store.CreateArtifactInput{
			ShareType:         artifact.ShareType(firstNonEmpty(r.URL.Query().Get("type"), string(artifact.TypeFile))),
			Title:             firstNonEmpty(title, f.name),
			Body:              f.file,
			DeclaredMediaType: f.media,
			Provenance:        prov,
			Access:            access,
			ExpiresAt:         expires,
		})
		if err != nil {
			s.writeError(w, r, mapUploadErr(err), nil)
			return
		}
		s.writeJSON(w, http.StatusCreated, s.toArtifactResponse(art))
		return
	}

	members := make([]store.MemberInput, 0, len(files))
	for _, f := range files {
		members = append(members, store.MemberInput{Name: f.name, Body: f.file, DeclaredMediaType: f.media})
	}
	art, err := s.store.CreateBundle(r.Context(), store.CreateBundleInput{
		Title:      title,
		Members:    members,
		Provenance: prov,
		Access:     access,
		ExpiresAt:  expires,
	})
	if err != nil {
		s.writeError(w, r, mapUploadErr(err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, s.toArtifactResponse(art))
}

// handleGet resolves an artifact's metadata + preview info. Unknown/expired ids
// return a uniform 404 (SPEC-0002 REQ "Artifact Lifecycle — Read").
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	art, err := s.store.GetByPublicID(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err, map[string]string{"id": id})
		return
	}
	s.writeJSON(w, http.StatusOK, s.toArtifactResponse(art))
}

// handleGetBody streams the raw body, re-verifiable against the stored SHA-256.
func (s *Server) handleGetBody(w http.ResponseWriter, r *http.Request) {
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

// handleDelete removes an artifact, owner-only.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
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
	p, ok := principalFrom(r.Context())
	if !ok {
		s.writeError(w, r, errs.ErrUnauthorized, nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := s.store.ListBin(r.Context(), p.ActorID, r.URL.Query().Get("cursor"), limit)
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

// mapUploadErr maps an http.MaxBytesReader overflow to the payload-too-large
// domain error so it renders as 413 rather than a generic 500.
func mapUploadErr(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errs.ErrTooLarge
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
