package webhook

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Captured Request Redaction
//
// A captured request is the least-trusted content Cairn stores, and it is
// link-readable, so Capture scans its query, headers and body for credentials
// before the ring-buffer insert and masks every hit as [REDACTED] (SPEC-0017
// RD-4). A capture is never refused: the sender is an anonymous third party
// that cannot act on a refusal, and the ingress response must not change
// (SPEC-0005 "Fixed Benign Response"). So every outcome that would reject a
// field on another surface stores something safe instead:
//
//   - a binary body (RD-6) is stored unchanged, flagged not_scanned_binary;
//   - a field over the scan cap (RD-7) is withheld, or stored unscanned and
//     flagged not_scanned_oversize (with a WARN) when the operator set
//     CAIRN_REDACTION_OVERSIZE=store_unscanned;
//   - a credential the masker cannot locate, or a scan that fails outright,
//     withholds the field: a notice replaces the raw bytes, and the field is
//     named in Request.Withheld. The raw bytes are never stored.
//
// The masked form is the only form that exists after scrub returns: it is
// what is inserted, what the hub fans out to SSE and MCP subscribers, and what
// every read returns. Nothing here logs, stores or returns a scanned value;
// logs carry the endpoint id, the field, a reason from a closed set and rule
// IDs (RD-9).
//
// Governing: ADR-0023, SPEC-0017 RD-1, RD-4, RD-6, RD-7, RD-9; ADR-0010,
// SPEC-0005 "Header Hygiene & Ephemerality as Containment"
//
// @joestump 09/26/2026 - Added for cairn#293.

// The captured fields scrub scans, and the only names Request.Withheld (and
// the redaction_withheld column's CHECK) can hold.
const (
	FieldQuery   = "query"
	FieldHeaders = "headers"
	FieldBody    = "body"
)

// Reasons a field is withheld, logged next to the field name.
const (
	withheldTooLarge   = "too_large_to_scan"
	withheldUnmaskable = "secret_unmaskable"
	withheldScanFailed = "scan_failed"
)

// withheldBodyNotice is stored in place of a body Cairn could not store
// safely. It starts with the mask so it reads as redacted wherever the body is
// shown.
const withheldBodyNotice = redact.Mask + " Cairn withheld this request body: "

var withheldBodyReasons = map[string]string{
	withheldTooLarge:   "it is larger than the credential scan limit.\n",
	withheldUnmaskable: "it holds a credential that could not be masked.\n",
	withheldScanFailed: "the credential scan failed.\n",
}

// sniffBytes is how much of a body RD-6's UTF-8 test reads.
const sniffBytes = 8 << 10

// textScanner is the part of *redact.Scanner Capture uses. Tests in this
// package substitute one that fails.
type textScanner interface {
	Text(ctx context.Context, field, in string, mode redact.Mode) (string, redact.Outcome, error)
}

// scrubbed is a capture after the scan: exactly what is stored and fanned out.
type scrubbed struct {
	query    string
	headers  map[string][]string
	body     []byte
	summary  redact.Summary
	withheld []string
}

// scrub scans a capture's query, sanitized headers and body in mask mode and
// returns what may be stored. It never fails: a field it cannot make safe is
// withheld. With no scanner configured (a wiring that predates it) the fields
// pass through and the outcome is the zero Summary, "unscanned". Header
// hygiene's credential mask applies either way.
func (s *Service) scrub(ctx context.Context, publicID string, in CaptureInput, headers map[string][]string) scrubbed {
	out := scrubbed{query: in.Query, headers: headers, body: in.Body}
	if s.scanner == nil {
		out.headers = maskCredentialHeaders(out.headers)
		return out
	}
	out.summary = redact.Summary{Status: redact.StatusClean}

	if in.Query != "" {
		masked, sum, ok := s.scanField(ctx, publicID, FieldQuery, in.Query)
		out.summary = out.summary.Merge(sum)
		if ok {
			out.query = masked
		} else {
			out.query = redact.Mask
			out.withheld = append(out.withheld, FieldQuery)
		}
	}

	if len(headers) > 0 {
		text, names := serializeHeaders(headers)
		masked, sum, ok := s.scanField(ctx, publicID, FieldHeaders, text)
		out.summary = out.summary.Merge(sum)
		var parsed map[string][]string
		if ok {
			parsed, ok = parseHeaders(masked, names)
		}
		if ok {
			out.headers = parsed
		} else {
			out.headers = maskAllHeaderValues(headers)
			out.withheld = append(out.withheld, FieldHeaders)
		}
	}
	out.headers = maskCredentialHeaders(out.headers)

	if len(in.Body) > 0 {
		if !isTextBody(in.ContentType, in.Body) {
			o := redact.Outcome{Status: redact.StatusNotScannedBinary}
			s.metrics.ObserveRedaction(metrics.SurfaceWebhook, o, nil)
			out.summary = out.summary.Merge(o.Summary())
		} else {
			masked, sum, ok := s.scanField(ctx, publicID, FieldBody, string(in.Body))
			out.summary = out.summary.Merge(sum)
			if ok {
				out.body = []byte(masked)
			} else {
				out.body = []byte(withheldBodyNotice + withheldBodyReasons[withholdReason(sum)])
				out.withheld = append(out.withheld, FieldBody)
			}
		}
	}

	// The rule IDs come from the scanner's own config, so this never fails in
	// practice. If it ever did, the status and count are still worth
	// recording, and the insert must not fail on the sender's behalf.
	if norm, err := out.summary.Normalized(); err == nil {
		out.summary = norm
	} else {
		out.summary.Rules = nil
	}
	return out
}

// scanField runs one field through the scanner in mask mode and counts the
// scan. ok is false when the field must be withheld; the returned Summary then
// still records what the scan found, and its Status says why (see
// withholdReason).
func (s *Service) scanField(ctx context.Context, publicID, field, in string) (string, redact.Summary, bool) {
	masked, o, err := s.scanner.Text(ctx, field, in, redact.ModeMask)
	s.metrics.ObserveRedaction(metrics.SurfaceWebhook, o, err)
	if err == nil {
		if o.Status == redact.StatusNotScannedOversize {
			// RD-7: loud each time. The size, never the content.
			s.log.WarnContext(ctx, "webhook: captured field stored WITHOUT a credential scan (CAIRN_REDACTION_OVERSIZE=store_unscanned)",
				"endpoint", publicID, "field", field, "size", len(in))
		}
		return masked, o.Summary(), true
	}

	var sum redact.Summary
	switch {
	case errors.Is(err, redact.ErrTooLargeToScan):
		sum = redact.Summary{Status: redact.StatusNotScannedOversize}
	case errors.Is(err, redact.ErrSecretDetected):
		sum = redact.Summary{Status: redact.StatusMasked, Count: o.Count, Rules: maps.Clone(o.Rules)}
	default:
		sum = redact.Summary{Status: redact.StatusUnscanned}
	}
	// The error is not logged: its text is safe by construction, but the
	// reason and rule IDs say everything an operator needs.
	s.log.WarnContext(ctx, "webhook: captured field withheld",
		"endpoint", publicID, "field", field, "reason", withholdReason(sum),
		"rules", slices.Sorted(maps.Keys(sum.Rules)))
	return "", sum, false
}

// withholdReason names why scanField withheld a field, from the Summary it
// recorded for it.
func withholdReason(sum redact.Summary) string {
	switch sum.Status {
	case redact.StatusNotScannedOversize:
		return withheldTooLarge
	case redact.StatusMasked:
		return withheldUnmaskable
	}
	return withheldScanFailed
}

// serializeHeaders renders headers as the "name: value" lines the scanner's
// header rules expect (an Authorization rule needs its label on the same
// line), one line per value, names sorted so the text is deterministic. names
// records each line's header, for parseHeaders.
func serializeHeaders(h map[string][]string) (string, []string) {
	var (
		b     strings.Builder
		names []string
	)
	for _, name := range slices.Sorted(maps.Keys(h)) {
		for _, v := range h[name] {
			if len(names) > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(v)
			names = append(names, name)
		}
	}
	return b.String(), names
}

// parseHeaders reverses serializeHeaders over the masked text. The mask never
// contains a newline, so masking keeps one line per value unless a masked
// range crossed a line or ate a header's label. Either way ok is false and
// the caller withholds every value rather than guess which bytes belong where.
func parseHeaders(masked string, names []string) (map[string][]string, bool) {
	lines := strings.Split(masked, "\n")
	if len(lines) != len(names) {
		return nil, false
	}
	out := make(map[string][]string, len(names))
	for i, line := range lines {
		label := names[i] + ": "
		if !strings.HasPrefix(line, label) {
			return nil, false
		}
		out[names[i]] = append(out[names[i]], line[len(label):])
	}
	return out, true
}

// maskAllHeaderValues keeps every header's name and replaces each value with
// the mask: the fail-closed form of headers that could not be scanned.
func maskAllHeaderValues(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, vals := range h {
		masked := make([]string, len(vals))
		for i := range vals {
			masked[i] = redact.Mask
		}
		out[name] = masked
	}
	return out
}

// isTextBody decides whether a captured body is scanned (SPEC-0017 RD-6). A
// textual declared type, or a first 8 KiB that is valid UTF-8 with no NUL,
// means text. Otherwise a body that starts with a known binary signature is
// binary. A body that is neither is scanned anyway: when in doubt, scan.
func isTextBody(contentType string, body []byte) bool {
	if textualMediaType(contentType) {
		return true
	}
	head := body
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
		// Do not count a rune the 8 KiB cut split in two as invalid.
		for i := 0; i < utf8.UTFMax-1 && !utf8.Valid(head); i++ {
			head = head[:len(head)-1]
		}
	}
	if utf8.Valid(head) && bytes.IndexByte(head, 0) < 0 {
		return true
	}
	return !hasBinarySignature(body)
}

// textualMediaType reports whether a declared content type is one RD-6 lists
// as text.
func textualMediaType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	switch {
	case strings.HasPrefix(mt, "text/"),
		strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/yaml", "application/toml",
		"application/javascript", "application/x-sh":
		return true
	}
	return false
}

// hasBinarySignature reports whether body starts with a signature net/http's
// content sniffer knows (images, audio, video, fonts, archives, PDF, wasm).
// Its catch-all for bytes it does not recognize, application/octet-stream, is
// not a signature.
func hasBinarySignature(body []byte) bool {
	ct := http.DetectContentType(body)
	return ct != "application/octet-stream" && !strings.HasPrefix(ct, "text/")
}
