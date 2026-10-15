package tracectx

import "testing"

// Governing: SPEC-0024 REQ "Trace Context on Create", REQ "Input Handling";
// ADR-0030.

const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestParseValid(t *testing.T) {
	ctx, ok := Parse(valid)
	if !ok {
		t.Fatalf("Parse(%q) rejected a valid traceparent", valid)
	}
	if got, want := ctx.TraceHex(), "4bf92f3577b34da6a3ce929d0e0e4736"; got != want {
		t.Errorf("TraceHex = %q, want %q", got, want)
	}
	if got, want := ctx.SpanHex(), "00f067aa0ba902b7"; got != want {
		t.Errorf("SpanHex = %q, want %q", got, want)
	}
}

// The SPEC-0024 round-trip scenario, in both directions.
func TestTraceIDRoundTripsAsHex(t *testing.T) {
	ctx, ok := Parse(valid)
	if !ok {
		t.Fatal("valid traceparent rejected")
	}
	store, ok := From(ctx.TraceID[:], ctx.SpanID[:])
	if !ok {
		t.Fatal("a context's own bytes must rebuild it")
	}
	if store.TraceHex() != ctx.TraceHex() || store.SpanHex() != ctx.SpanHex() {
		t.Fatal("From did not round-trip the identifiers")
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"version 01", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"version ff", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"uppercase trace", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"},
		{"uppercase span", "00-4bf92f3577b34da6a3ce929d0e0e4736-00F067AA0BA902B7-01"},
		{"uppercase flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0F"},
		{"non hex", "00-zbf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"zero trace", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		{"zero span", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"},
		{"short trace", "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01"},
		{"long trace", "00-4bf92f3577b34da6a3ce929d0e0e47366-00f067aa0ba902b7-01"},
		{"short span", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-01"},
		{"missing flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7"},
		{"trailing junk", valid + "-x"},
		{"leading space", " " + valid},
		{"no separators", "004bf92f3577b34da6a3ce929d0e0e473600f067aa0ba902b701"},
		{"tracestate appended", valid + ", foo=bar"},
	}
	for _, tc := range cases {
		if _, ok := Parse(tc.in); ok {
			t.Errorf("%s: Parse(%q) accepted a value it must ignore", tc.name, tc.in)
		}
	}
}

// A traceparent that is 56 bytes or 54 must be ignored, never truncated to fit:
// SPEC-0024 bounds the header at 55 bytes and says a longer one is ignored.
func TestParseLengthBoundary(t *testing.T) {
	if Length != 55 {
		t.Fatalf("Length = %d, the wire form is 55 bytes", Length)
	}
	for _, in := range []string{valid[:54], valid + "0"} {
		if _, ok := Parse(in); ok {
			t.Errorf("Parse accepted a %d-byte value", len(in))
		}
	}
}

func TestFromRequiresExactWidths(t *testing.T) {
	if _, ok := From(make([]byte, 15), make([]byte, 8)); ok {
		t.Error("From accepted a 15-byte trace id")
	}
	if _, ok := From(make([]byte, 16), make([]byte, 9)); ok {
		t.Error("From accepted a 9-byte span id")
	}
	if _, ok := From(nil, nil); ok {
		t.Error("From accepted no bytes at all")
	}
}
