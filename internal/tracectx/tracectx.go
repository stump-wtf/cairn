// Package tracectx parses the W3C trace context a create may carry, so every
// surface reads the same value the same way.
//
// A create's `traceparent` is the only place Cairn learns which run produced an
// artifact. It is optional and it is never trusted for anything but provenance:
// a missing or malformed value is ignored rather than rejected, because failing
// an upload over an observability header would turn a telemetry bug into a
// data-loss bug.
//
// Parsing is strict, per SPEC-0024 REQ "Input Handling": exactly 55 bytes in
// the form 00-<32 hex>-<16 hex>-<2 hex>, lowercase hex only, version 00, and
// neither identifier all zeros (an all-zero id is invalid on the wire, and a
// zero trace would group every sloppy client's spans into one fake run).
// `tracestate` is ignored: nothing in Cairn consumes it.
//
// Governing: ADR-0030 (Artifact Relations and Trace Context),
// SPEC-0024 REQ "Trace Context on Create"
package tracectx

const (
	// Length is the exact wire length of a version-00 traceparent:
	// "00-" + 32 + "-" + 16 + "-" + 2.
	Length = 55
	// TraceLen and SpanLen are the raw identifier widths stored on the artifact.
	TraceLen = 16
	SpanLen  = 8
)

// Context is a parsed trace context. TraceID is the 16-byte trace and SpanID
// the 8-byte parent span — the span of the caller that made this request, which
// is what lets an artifact hang off the span that produced it.
type Context struct {
	TraceID [TraceLen]byte
	SpanID  [SpanLen]byte
}

// Parse reads a traceparent. ok is false for anything that is not a valid
// version-00 value, and the caller then stores nothing: an absent header and a
// malformed one mean the same thing.
func Parse(s string) (ctx Context, ok bool) {
	if len(s) != Length || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return ctx, false
	}
	if s[0] != '0' || s[1] != '0' {
		return ctx, false // only version 00 is defined here
	}
	// flags (s[53:55]) are validated as hex and otherwise ignored: the sampled
	// bit changes nothing about what Cairn stores.
	trace, ok := decodeHex(s[3:35])
	if !ok {
		return ctx, false
	}
	span, ok := decodeHex(s[36:52])
	if !ok {
		return ctx, false
	}
	if _, ok := decodeHex(s[53:55]); !ok {
		return ctx, false
	}
	if isZero(trace) || isZero(span) {
		return ctx, false
	}
	copy(ctx.TraceID[:], trace)
	copy(ctx.SpanID[:], span)
	return ctx, true
}

// TraceHex is the trace id as 32 lowercase hex characters, the form every read
// surface and artifact.created put on the wire (SPEC-0024 REQ "Trace Id on Read
// and in Events").
func (c Context) TraceHex() string { return encodeHex(c.TraceID[:]) }

// From rebuilds a Context from the stored bytes, so a read returns the trace the
// create stored rather than one re-parsed from hex. ok is false unless both
// identifiers have their exact width — a short read is a bug elsewhere, and
// reporting it is better than padding a trace that never existed.
func From(traceID, parentSpanID []byte) (ctx Context, ok bool) {
	if len(traceID) != TraceLen || len(parentSpanID) != SpanLen {
		return ctx, false
	}
	copy(ctx.TraceID[:], traceID)
	copy(ctx.SpanID[:], parentSpanID)
	return ctx, true
}

// SpanHex is the parent span id as 16 lowercase hex characters. It is stored
// for a later join and is not put on the wire in this slice.
func (c Context) SpanHex() string { return encodeHex(c.SpanID[:]) }

const hexDigits = "0123456789abcdef"

// decodeHex decodes a run of lowercase hex. Uppercase is rejected rather than
// folded, so the stored bytes are byte-for-byte what the client sent.
func decodeHex(s string) ([]byte, bool) {
	if len(s)%2 != 0 {
		return nil, false
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok := hexValue(s[2*i])
		if !ok {
			return nil, false
		}
		lo, ok := hexValue(s[2*i+1])
		if !ok {
			return nil, false
		}
		out[i] = hi<<4 | lo
	}
	return out, true
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

func encodeHex(b []byte) string {
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = hexDigits[c>>4]
		out[2*i+1] = hexDigits[c&0x0f]
	}
	return string(out)
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
