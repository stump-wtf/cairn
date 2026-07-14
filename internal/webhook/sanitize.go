package webhook

import "strings"

// sensitiveHeaders are dropped outright on capture: replayable credentials a
// stored, sanitized projection must never carry forward (SPEC-0005 "Header
// Hygiene & Ephemerality as Containment": "the stored request is a sanitized
// projection, not a replayable credential dump").
var sensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
}

// hopByHopHeaders are the RFC 7230 §6.1 connection-scoped headers, meaningless
// once captured and replayed as stored data, so they are dropped alongside the
// sensitive set (SPEC-0005 "Header Hygiene").
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// sanitizeHeaders normalizes header names to lowercase and drops every
// sensitive or hop-by-hop header, returning a fresh map so the caller's
// original headers (which the future ingress reads off the raw *http.Request)
// are never mutated. A nil or empty input yields a nil map, which persists as
// the migration's `'{}'::jsonb` default.
func sanitizeHeaders(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for name, values := range in {
		lower := strings.ToLower(name)
		if _, drop := sensitiveHeaders[lower]; drop {
			continue
		}
		if _, drop := hopByHopHeaders[lower]; drop {
			continue
		}
		if len(values) == 0 {
			continue
		}
		cp := make([]string, len(values))
		copy(cp, values)
		out[lower] = cp
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
