package webhook

import (
	"strings"

	"github.com/stump-wtf/cairn/internal/redact"
)

// sensitiveHeaders are dropped outright on capture: replayable credentials a
// stored, sanitized projection must never carry forward (SPEC-0005 "Header
// Hygiene & Ephemerality as Containment": "the stored request is a sanitized
// projection, not a replayable credential dump").
//
// Authorization is not here: it is kept by name with its value masked
// (credentialHeaders), because SPEC-0017 RD-4 wants the stored capture to show
// that the sender presented one.
var sensitiveHeaders = map[string]struct{}{
	"cookie":              {},
	"set-cookie":          {},
	"proxy-authorization": {},
}

// credentialHeaders are kept by name and never by value. The scanner masks a
// recognizable credential in them first, and counts it; maskCredentialHeaders
// then masks whatever value is left, recognized or not, so header hygiene
// never depends on a detector rule matching.
var credentialHeaders = map[string]struct{}{
	"authorization": {},
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

// lineBreaks turns CR and LF into spaces. A header line cannot carry them on
// the wire, but a direct Capture caller could, and the scanner reads one
// header value per line (serializeHeaders).
var lineBreaks = strings.NewReplacer("\r", " ", "\n", " ")

// sanitizeHeaders normalizes header names to lowercase and drops every
// sensitive or hop-by-hop header, returning a fresh map so the caller's
// original headers (the open ingress passes the raw *http.Request's Header
// map straight through) are never mutated. A nil or empty input yields a nil
// map, which persists as the migration's `'{}'::jsonb` default.
//
// Credential headers come back with their raw values, for the scanner to see
// and count. Capture always passes the result through scrub, which masks them
// (maskCredentialHeaders) before anything is stored or fanned out.
func sanitizeHeaders(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for name, values := range in {
		lower := lineBreaks.Replace(strings.ToLower(name))
		if _, drop := sensitiveHeaders[lower]; drop {
			continue
		}
		if _, drop := hopByHopHeaders[lower]; drop {
			continue
		}
		for _, v := range values {
			out[lower] = append(out[lower], lineBreaks.Replace(v))
		}
	}
	for name, values := range out {
		if len(values) == 0 {
			delete(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// maskCredentialHeaders replaces the value of every credential header, keeping
// its scheme word ("Bearer [REDACTED]"), or the whole value when it has none.
// It rewrites h in place and returns it. It is idempotent, and it does not
// trust a value that already contains the mask: the sender controls the value,
// so "Bearer [REDACTED]xyz" is masked again like any other.
func maskCredentialHeaders(h map[string][]string) map[string][]string {
	for name, vals := range h {
		if _, ok := credentialHeaders[name]; !ok {
			continue
		}
		for i, v := range vals {
			vals[i] = maskCredential(v)
		}
	}
	return h
}

func maskCredential(v string) string {
	scheme, _, found := strings.Cut(strings.TrimSpace(v), " ")
	if found && isSchemeWord(scheme) {
		return scheme + " " + redact.Mask
	}
	return redact.Mask
}

// isSchemeWord reports whether s is a known auth scheme, case-insensitively.
// The first word is kept verbatim, so it must come from a closed list: a
// shape test ("a short run of letters and digits") also passes an API key
// sent as "Authorization: <key> <signature>", and would store the key.
func isSchemeWord(s string) bool {
	_, ok := authSchemes[strings.ToLower(s)]
	return ok
}

// authSchemes is the IANA HTTP Authentication Scheme Registry plus the
// unregistered schemes webhook senders commonly use.
var authSchemes = map[string]struct{}{
	"basic": {}, "bearer": {}, "concealed": {}, "digest": {}, "dpop": {}, "gnap": {},
	"hoba": {}, "mutual": {}, "negotiate": {}, "ntlm": {}, "oauth": {},
	"privatetoken": {}, "scram-sha-1": {}, "scram-sha-256": {}, "vapid": {},
	"apikey": {}, "aws4-hmac-sha256": {}, "sharedkey": {}, "token": {},
}
