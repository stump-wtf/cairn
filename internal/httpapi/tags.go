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

// tagErrorDetails names the rejected input in the error envelope. The REST
// surface never echoes a validation message (messageFor maps every code to a
// fixed string), so without this a caller could not tell a bad tag from a bad
// TTL or body.
func tagErrorDetails() map[string]string {
	return map[string]string{"field": "tag"}
}

// tagSet accumulates tags from a request. Each tag is validated and
// deduplicated as it arrives and the distinct count is bounded, so the
// multipart path — where the number of fields is otherwise bounded only by the
// body — stops at the first tag too many rather than buffering them all. The
// store normalizes the assembled list again at its single create choke point,
// which is what the MCP surface relies on.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0002 REQ "Artifact
// Tags"
type tagSet struct {
	tags []string
	seen map[string]struct{}
}

// addList adds a comma-separated list. Whitespace around a tag is trimmed and
// empty items are skipped, so "a, b," and "a,b" mean the same thing.
func (ts *tagSet) addList(list string) error {
	for _, item := range strings.Split(list, ",") {
		t := strings.TrimSpace(item)
		if t == "" {
			continue
		}
		if err := artifact.ValidateTag(t); err != nil {
			return err
		}
		if _, dup := ts.seen[t]; dup {
			continue
		}
		if len(ts.tags) == artifact.MaxTags {
			return errs.Validationf("tags: more than the maximum of %d distinct tags", artifact.MaxTags)
		}
		if ts.seen == nil {
			ts.seen = map[string]struct{}{}
		}
		ts.seen[t] = struct{}{}
		ts.tags = append(ts.tags, t)
	}
	return nil
}

// addQuery adds every ?tag= parameter.
func (ts *tagSet) addQuery(r *http.Request) error {
	for _, list := range r.URL.Query()["tag"] {
		if err := ts.addList(list); err != nil {
			return err
		}
	}
	return nil
}

// addFromRequest adds ?tag= parameters, then X-Cairn-Tags headers.
func (ts *tagSet) addFromRequest(r *http.Request) error {
	if err := ts.addQuery(r); err != nil {
		return err
	}
	for _, list := range r.Header.Values(tagHeader) {
		if err := ts.addList(list); err != nil {
			return err
		}
	}
	return nil
}

// addFormField reads one multipart `tag` field.
func (ts *tagSet) addFormField(part io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(part, int64(maxTagFieldBytes)+1))
	if err != nil {
		return errs.Validationf("multipart: malformed tag field")
	}
	if len(b) > maxTagFieldBytes {
		return errs.Validationf("tags: a tag field exceeds %d bytes", maxTagFieldBytes)
	}
	return ts.addList(string(b))
}
