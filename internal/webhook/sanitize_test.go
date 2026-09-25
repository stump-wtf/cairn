package webhook

import (
	"reflect"
	"testing"
)

// TestSanitizeHeadersDropsSensitiveAndHopByHop proves capture never persists a
// replayable credential set or connection-scoped noise (SPEC-0005 "Header
// Hygiene & Ephemerality as Containment").
func TestSanitizeHeadersDropsSensitiveAndHopByHop(t *testing.T) {
	in := map[string][]string{
		"Authorization":       {"Bearer secret"},
		"Cookie":              {"session=abc"},
		"Set-Cookie":          {"session=abc; Path=/"},
		"Proxy-Authorization": {"Basic xyz"},
		"Connection":          {"keep-alive"},
		"Transfer-Encoding":   {"chunked"},
		"Content-Type":        {"application/json"},
		"X-Request-Id":        {"r-1", "r-2"},
	}
	got := sanitizeHeaders(in)
	want := map[string][]string{
		"content-type": {"application/json"},
		"x-request-id": {"r-1", "r-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeHeaders = %#v, want %#v", got, want)
	}
}

// TestSanitizeHeadersEmpty proves an empty/nil input yields a nil map (so the
// caller persists the migration's `'{}'::jsonb` default) rather than an empty
// non-nil map.
func TestSanitizeHeadersEmpty(t *testing.T) {
	if got := sanitizeHeaders(nil); got != nil {
		t.Fatalf("sanitizeHeaders(nil) = %#v, want nil", got)
	}
	if got := sanitizeHeaders(map[string][]string{"Authorization": {"x"}}); got != nil {
		t.Fatalf("sanitizeHeaders(all-dropped) = %#v, want nil", got)
	}
}

// TestSanitizeHeadersDoesNotMutateInput proves the sanitizer returns a fresh
// map and slices, never mutating the caller's original headers (the future
// ingress reads these straight off the raw *http.Request).
func TestSanitizeHeadersDoesNotMutateInput(t *testing.T) {
	in := map[string][]string{"X-Foo": {"bar"}}
	out := sanitizeHeaders(in)
	out["x-foo"][0] = "mutated"
	if in["X-Foo"][0] != "bar" {
		t.Fatalf("sanitizeHeaders mutated caller input: %v", in)
	}
}

func TestEndpointInputValidate(t *testing.T) {
	base := func() EndpointInput {
		return EndpointInput{
			Provenance: validProvenance(),
			Access:     validAccess(),
			ExpiresAt:  future(),
		}
	}
	if err := base().validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*EndpointInput)
	}{
		{"missing channel", func(in *EndpointInput) { in.Provenance.Channel = "" }},
		{"missing actor", func(in *EndpointInput) { in.Provenance.ActorID = "" }},
		{"missing captured_at", func(in *EndpointInput) { in.Provenance.CapturedAt = zeroTime }},
		{"missing owner", func(in *EndpointInput) { in.Access.OwnerUserID = "" }},
		{"missing visibility", func(in *EndpointInput) { in.Access.Visibility = "" }},
		{"missing expiry", func(in *EndpointInput) { in.ExpiresAt = zeroTime }},
		{"negative cap", func(in *EndpointInput) { in.RequestCap = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mut(&in)
			if err := in.validate(); err == nil {
				t.Fatalf("%s: want validation error, got nil", tc.name)
			}
		})
	}
}

func TestCaptureInputValidate(t *testing.T) {
	valid := CaptureInput{Method: "POST", Status: 200}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid capture rejected: %v", err)
	}
	if err := (CaptureInput{Status: 200}).validate(); err == nil {
		t.Fatal("empty method: want validation error, got nil")
	}
	if err := (CaptureInput{Method: "GET", Status: 0}).validate(); err == nil {
		t.Fatal("status 0: want validation error, got nil")
	}
	if err := (CaptureInput{Method: "GET", Status: 700}).validate(); err == nil {
		t.Fatal("status 700: want validation error, got nil")
	}
}
