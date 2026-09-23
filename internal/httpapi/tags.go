package httpapi

import (
	"io"
	"net/http"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
)

// tagHeader carries a comma-separated tag list on a REST create. The comma is a
// safe separator because no tag may contain one — which also makes the header
// immune to an intermediary folding repeated header lines into one
// comma-joined value (RFC 9110). The repeatable ?tag= query parameter, and the
// repeatable `tag` form field on a multipart create, take the same
// comma-separated form; every source on one request is merged.
const tagHeader = "X-Cairn-Tags"

// maxTagFieldBytes bounds one multipart `tag` field before it is split: a full
// complement of maximum-length tags plus separators. Anything longer can only
// hold an over-long or surplus tag, so it is rejected whole rather than cut.
const maxTagFieldBytes = artifact.MaxTags * (artifact.MaxTagBytes + len(","))

// tagField is the caller-facing field every REST tag source reports its
// violations under, whichever source carried the tag; the violation's location
// says which (query, header or form).
const tagField = "tag"

// tagSet accumulates tags from a request. Each tag is validated and
// deduplicated as it arrives and the distinct count is bounded, so the
// multipart path — where the number of fields is otherwise bounded only by the
// body — never buffers more than MaxTags tags. Every bad tag, from every
// source, is recorded rather than only the first, and err reports them
// together. The store normalizes the assembled list again at its single create
// choke point, which is what the MCP surface relies on.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
// Tags", ADR-0025, SPEC-0019 VE-4
type tagSet struct {
	tags    []string
	seen    map[string]struct{}
	bad     []*errs.Invalid
	tooMany errs.Location // where the first tag past MaxTags arrived, or ""
}

// err is every violation recorded so far, or nil.
func (ts *tagSet) err() error {
	bad := ts.bad
	if ts.tooMany != "" {
		bad = append(bad[:len(bad):len(bad)], artifact.TooManyTags(tagField, ts.tooMany))
	}
	if inv := errs.Join(bad...); inv != nil {
		return inv
	}
	return nil
}

// addList adds a comma-separated list. Whitespace around a tag is trimmed and
// empty items are skipped, so "a, b," and "a,b" mean the same thing.
func (ts *tagSet) addList(list string, loc errs.Location) {
	for _, item := range strings.Split(list, ",") {
		t := strings.TrimSpace(item)
		if t == "" {
			continue
		}
		if inv := artifact.CheckTag(t, tagField, loc); inv != nil {
			if len(ts.bad) < errs.MaxViolations {
				ts.bad = append(ts.bad, inv)
			}
			continue
		}
		if _, dup := ts.seen[t]; dup {
			continue
		}
		if len(ts.tags) == artifact.MaxTags {
			if ts.tooMany == "" {
				ts.tooMany = loc
			}
			continue
		}
		if ts.seen == nil {
			ts.seen = map[string]struct{}{}
		}
		ts.seen[t] = struct{}{}
		ts.tags = append(ts.tags, t)
	}
}

// addQuery adds every ?tag= parameter.
func (ts *tagSet) addQuery(r *http.Request) error {
	for _, list := range r.URL.Query()["tag"] {
		ts.addList(list, errs.LocQuery)
	}
	return ts.err()
}

// addFromRequest adds ?tag= parameters, then X-Cairn-Tags headers.
func (ts *tagSet) addFromRequest(r *http.Request) error {
	for _, list := range r.URL.Query()["tag"] {
		ts.addList(list, errs.LocQuery)
	}
	for _, list := range r.Header.Values(tagHeader) {
		ts.addList(list, errs.LocHeader)
	}
	return ts.err()
}

// addFormField reads one multipart `tag` field. A field too long to hold only
// valid tags is rejected whole, rather than cut.
func (ts *tagSet) addFormField(part io.Reader) {
	b, err := io.ReadAll(io.LimitReader(part, int64(maxTagFieldBytes)+1))
	switch {
	case err != nil:
		ts.bad = append(ts.bad, errs.Violate(tagField, errs.LocForm, errs.ReasonInvalidFormat))
	case len(b) > maxTagFieldBytes:
		ts.bad = append(ts.bad, errs.Violate(tagField, errs.LocForm, errs.ReasonTooLong,
			errs.WithLimit(maxTagFieldBytes, errs.UnitBytes)))
	default:
		ts.addList(string(b), errs.LocForm)
	}
}
