package httpapi

import (
	"io"
	"net/http"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/store"
)

// The REST create's validators for what the caller names outside the body:
// its share type, its title and the multipart envelope. Each reports on the
// field and location the caller actually used, so a violation says which knob
// to turn. The store re-checks the type and title at its single create choke
// point, which is what the MCP surface relies on.
//
// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-1, VE-4,
// VE-6; SPEC-0002 REQ "Artifact Lifecycle — Create", REQ "Bundles with N
// Members"

// Where a single-body create's share type and title may be carried. The query
// parameter wins over the header, as it always has.
const (
	typeHeader  = "X-Cairn-Type"
	titleHeader = "X-Cairn-Title"
)

// requestedShareType reads the declared share type, defaulting to the generic
// file type, and checks it against the registry.
func (s *Server) requestedShareType(r *http.Request) (artifact.ShareType, error) {
	field, loc, raw := "type", errs.LocQuery, r.URL.Query().Get("type")
	if raw == "" {
		field, loc, raw = typeHeader, errs.LocHeader, r.Header.Get(typeHeader)
	}
	if raw == "" {
		return artifact.TypeFile, nil
	}
	t := artifact.ShareType(raw)
	if inv := s.reg.CheckCreateType(t, field, loc); inv != nil {
		return "", inv
	}
	return t, nil
}

// multipartShareType is requestedShareType for a single-file multipart create,
// which reads the type from the query only.
func (s *Server) multipartShareType(r *http.Request) (artifact.ShareType, error) {
	raw := r.URL.Query().Get("type")
	if raw == "" {
		return artifact.TypeFile, nil
	}
	t := artifact.ShareType(raw)
	if inv := s.reg.CheckCreateType(t, "type", errs.LocQuery); inv != nil {
		return "", inv
	}
	return t, nil
}

// requestedTitle reads the optional title from ?title= or X-Cairn-Title.
func requestedTitle(r *http.Request) (string, error) {
	field, loc, title := "title", errs.LocQuery, r.URL.Query().Get("title")
	if title == "" {
		field, loc, title = titleHeader, errs.LocHeader, r.Header.Get(titleHeader)
	}
	if inv := artifact.CheckTitle(title, field, loc); inv != nil {
		return "", inv
	}
	return title, nil
}

// readTitleField reads a multipart `title` field. A field over the cap is
// rejected whole rather than cut, and at most one byte past the cap is read.
func readTitleField(part io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, artifact.MaxTitleBytes+1))
	if err != nil {
		return "", errMalformedMultipart(err)
	}
	if inv := artifact.CheckTitle(string(b), "title", errs.LocForm); inv != nil {
		return "", inv
	}
	return string(b), nil
}

// errMissingBoundary is a multipart Content-Type with no boundary parameter.
func errMissingBoundary() error {
	return errs.Violate("Content-Type", errs.LocHeader, errs.ReasonInvalidFormat,
		errs.WithExpect("multipart/form-data with a boundary parameter"))
}

// errMalformedMultipart is a multipart body that cannot be parsed into parts.
// The parser's own error is kept as the cause, for the log line only.
func errMalformedMultipart(cause error) error {
	return errs.Violate("body", errs.LocBody, errs.ReasonInvalidFormat,
		errs.WithExpect("a well-formed multipart/form-data body")).Because(cause)
}

// partReader remembers whether a spooling copy failed reading the part, which
// the client caused (a body cut off mid-part), rather than writing the spool
// file, which it did not.
type partReader struct {
	r       io.Reader
	readErr bool
}

func (p *partReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && err != io.EOF {
		p.readErr = true
	}
	return n, err
}

// classify maps a failed spooling copy to the malformed-body violation when the
// part itself could not be read, and leaves a spool write failure internal.
func (p *partReader) classify(err error) error {
	if p.readErr {
		return errMalformedMultipart(err)
	}
	return err
}

// errNoFileParts is a multipart create carrying no file part at all.
func errNoFileParts() error {
	return errs.Violate("file", errs.LocForm, errs.ReasonRequired)
}

// memberChecks accumulates the violations of a multipart create's file parts,
// in part order, while they spool. They apply only once the upload turns out
// to be a bundle (two or more files): a single file is the artifact's body,
// whose oversize is reported on `body`, and whose name only titles it.
type memberChecks struct {
	n        int // file parts seen
	names    store.MemberNames
	bad      []*errs.Invalid
	oversize bool // some part exceeded the upload cap
}

// add records file part i: its name, and whether it exceeded the cap limit.
func (m *memberChecks) add(name string, tooLarge bool, limit int64) {
	i := m.n
	m.n++
	m.record(m.names.Check(i, name, errs.LocForm))
	if tooLarge {
		m.oversize = true
		m.record(store.MemberTooLarge(i, errs.LocForm, limit))
	}
}

func (m *memberChecks) record(inv *errs.Invalid) {
	if inv != nil && len(m.bad) < errs.MaxViolations {
		m.bad = append(m.bad, inv)
	}
}

// tooMany reports whether the parts seen already exceed a bundle's bound.
func (m *memberChecks) tooMany() bool { return m.n > store.MaxBundleMembers }

// err is the recorded member violations for a bundle, or nil. The count comes
// first, so the MaxViolations bound can never drop it.
func (m *memberChecks) err() error {
	var bad []*errs.Invalid
	if m.tooMany() {
		bad = append(bad, store.TooManyMembers(errs.LocForm))
	}
	if inv := errs.Join(append(bad, m.bad...)...); inv != nil {
		return inv
	}
	return nil
}
