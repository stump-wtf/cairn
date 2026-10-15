package httpapi

import (
	"io"
	"net/http"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/tracectx"
)

// relationHeader carries one relation on a REST create, repeated per relation:
// `X-Cairn-Relation: reply_to=a1b2c3`. A header per relation rather than one
// comma-joined list is what SPEC-0024 specifies here, and it also survives an
// intermediary folding repeated header lines into one value only if a client
// sends one relation per line, which is the shape the CLI uses.
//
// The repeatable ?relation= query parameter and the repeatable `relation` form
// field on a multipart create carry the same `<kind>=<id>` pair; every source on
// one request is merged. A separator is a comma nowhere in this vocabulary, so
// parsing never has to guess whether a value carried one relation or several.
//
// Governing: SPEC-0024 REQ "Declaring Relations on Create".
const relationHeader = "X-Cairn-Relation"

// traceparentHeader is the W3C trace context header a create may carry. It is
// read here and parsed nowhere else on this surface; an invalid value is ignored
// rather than rejected, because a broken telemetry header must not fail an
// upload.
const traceparentHeader = "traceparent"

// relationField is the caller-facing field every REST relation source reports
// its violations under, whichever source carried the relation; the violation's
// location says which (query, header or form). This mirrors tagField.
const relationField = "relation"

// maxRelationEntryBytes bounds one `relation=<kind>=<id>` entry before parsing:
// the longest kind, a separator, and an id at the store's bound. Longer can only
// mean a malformed entry, so it is rejected whole rather than cut.
const maxRelationEntryBytes = len("derived_from") + 1 + 32

// maxRelationFieldBytes bounds one multipart `relation` field.
const maxRelationFieldBytes = artifact.MaxRelations * (maxRelationEntryBytes + len(","))

// relationSet accumulates relations from a request, the way tagSet accumulates
// tags: each entry is parsed and validated as it arrives, duplicates on
// (kind, id) are dropped keeping first-occurrence order, and the distinct count
// is bounded, so the multipart path — where the number of fields is otherwise
// bounded only by the body — never buffers more than MaxRelations relations.
// Every bad entry, from every source, is recorded rather than only the first.
//
// Governing: SPEC-0024 REQ "Relation Kinds and Limits"; ADR-0025,
// SPEC-0019 VE-4
type relationSet struct {
	relations []artifact.Relation
	seen      map[artifact.Relation]struct{}
	bad       []*errs.Invalid
	tooMany   errs.Location // where the first relation past MaxRelations arrived, or ""
}

// err is every violation recorded so far, or nil.
func (rs *relationSet) err() error {
	bad := rs.bad
	if rs.tooMany != "" {
		bad = append(bad[:len(bad):len(bad)], artifact.TooManyRelations(relationField, rs.tooMany))
	}
	if inv := errs.Join(bad...); inv != nil {
		return inv
	}
	return nil
}

// record keeps a violation, up to errs.MaxViolations of them, so a body carrying
// any number of relation fields cannot turn its own rejection into a large
// response.
func (rs *relationSet) record(inv *errs.Invalid) {
	if len(rs.bad) < errs.MaxViolations {
		rs.bad = append(rs.bad, inv)
	}
}

// addEntry parses and adds one `<kind>=<id>` entry. `=` is the documented
// separator; `:` is accepted too, because the CLI's flags and every other
// key=value surface in Cairn have used both, and a client that guessed one
// should get a "no such relation" answer rather than a parse error. Surrounding
// whitespace is trimmed; an empty entry is skipped, so a trailing separator on a
// joined value costs nothing.
func (rs *relationSet) addEntry(entry string, loc errs.Location) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	if len(entry) > maxRelationEntryBytes {
		rs.record(errs.Violate(relationField, loc, errs.ReasonTooLong,
			errs.WithValue(entry), errs.WithLimit(maxRelationEntryBytes, errs.UnitBytes)))
		return
	}
	i := strings.IndexAny(entry, "=:")
	if i < 0 {
		rs.record(errs.Violate(relationField, loc, errs.ReasonInvalidFormat,
			errs.WithValue(entry), errs.WithExpect("kind=identifier")))
		return
	}
	rel := artifact.Relation{Kind: artifact.RelationKind(entry[:i]), ID: entry[i+1:]}
	if inv := artifact.CheckRelation(rel, relationField, loc); inv != nil {
		rs.record(inv)
		return
	}
	if _, dup := rs.seen[rel]; dup {
		return
	}
	if len(rs.relations) == artifact.MaxRelations {
		if rs.tooMany == "" {
			rs.tooMany = loc
		}
		return
	}
	if rs.seen == nil {
		rs.seen = map[artifact.Relation]struct{}{}
	}
	rs.seen[rel] = struct{}{}
	rs.relations = append(rs.relations, rel)
}

// addList adds a comma-separated list of entries.
func (rs *relationSet) addList(list string, loc errs.Location) {
	for _, entry := range strings.Split(list, ",") {
		rs.addEntry(entry, loc)
	}
}

// addFromRequest adds every ?relation= parameter, then every X-Cairn-Relation
// header value.
func (rs *relationSet) addFromRequest(r *http.Request) error {
	for _, list := range r.URL.Query()["relation"] {
		rs.addList(list, errs.LocQuery)
	}
	for _, list := range r.Header.Values(relationHeader) {
		rs.addList(list, errs.LocHeader)
	}
	return rs.err()
}

// addFormField reads one multipart `relation` field. A field too long to hold
// only valid entries is rejected whole, rather than cut.
func (rs *relationSet) addFormField(part io.Reader) {
	b, err := io.ReadAll(io.LimitReader(part, int64(maxRelationFieldBytes)+1))
	switch {
	case err != nil:
		rs.record(errs.Violate(relationField, errs.LocForm, errs.ReasonInvalidFormat))
	case len(b) > maxRelationFieldBytes:
		rs.record(errs.Violate(relationField, errs.LocForm, errs.ReasonTooLong,
			errs.WithLimit(maxRelationFieldBytes, errs.UnitBytes)))
	default:
		rs.addList(string(b), errs.LocForm)
	}
}

// requestTrace reads the create's trace context from the traceparent header.
// Absent and malformed mean the same thing: no trace. tracestate is ignored —
// nothing in Cairn consumes it.
//
// Governing: ADR-0030, SPEC-0024 REQ "Trace Context on Create".
func requestTrace(r *http.Request) *tracectx.Context {
	ctx, ok := tracectx.Parse(r.Header.Get(traceparentHeader))
	if !ok {
		return nil
	}
	return &ctx
}
