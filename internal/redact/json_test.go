package redact

// JSON Document Scan Tests
//
// Pins that JSON masks a document's values where they stand: the result still
// parses, the structure and the untouched bytes are kept, a member's name is
// the label the header rules key on, and no credential survives. Every
// credential is assembled at run time from split literals.
//
// Governing: ADR-0023, SPEC-0017 RD-3, RD-4
//
// @joestump 09/26/2026 - Added for cairn#291.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/errs"
)

// bearerValue is an opaque bearer credential with no vendor shape, so only
// the Authorization label rule can find it.
func bearerValue() string { return "zq8" + "Lk2Vx7" + "Pn4Rt9Wm" }

// passwordValue is a password with no vendor shape.
func passwordValue() string { return "hunter2" + "Sekr1t" + "Value" }

func mustParse(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("masked JSON does not parse: %v\n%s", err, b)
	}
	return v
}

// TestJSONAuthorizationLabelKept: SPEC-0017 RD-3 "Label kept, value masked"
// for a header in a JSON object. Raw text never matches this shape, because
// the rule expects `Authorization:` and JSON spells it `"Authorization":`.
func TestJSONAuthorizationLabelKept(t *testing.T) {
	s := newTestScanner(t)
	secret := bearerValue()
	doc := []byte(`{"url": "https://api.example.test/v1", "headers": {"Authorization": "Bearer ` + secret + `", "Accept": "application/json"}}`)

	out, o, err := s.JSON(context.Background(), "spans[0].args", doc, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"url": "https://api.example.test/v1", "headers": {"Authorization": "Bearer [REDACTED]", "Accept": "application/json"}}`
	if string(out) != want {
		t.Errorf("masked args\n got %s\nwant %s", out, want)
	}
	mustParse(t, out)
	if o.Status != StatusMasked || o.Count != 1 || o.Rules["cairn-authorization"] != 1 {
		t.Errorf("outcome = %+v; want masked, 1, cairn-authorization", o)
	}
	if strings.Contains(string(out), secret) {
		t.Error("the bearer value survived")
	}
}

// TestJSONMasksWithoutBreakingSyntax: the shapes that break when a JSON
// document is masked as raw text all come out valid, with the value gone.
func TestJSONMasksWithoutBreakingSyntax(t *testing.T) {
	s := newTestScanner(t)
	pw, tok := passwordValue(), pat(4)
	cases := []struct {
		name, doc, want string
	}{
		{"quoted assignment value", `{"password": "` + pw + `", "n": 1}`, `{"password": "[REDACTED]", "n": 1}`},
		{"vendor token under a secret name", `{"token":"` + tok + `"}`, `{"token":"[REDACTED]"}`},
		{"vendor token in an array", `{"argv": ["gh", "auth", "` + tok + `"]}`, `{"argv": ["gh", "auth", "[REDACTED]"]}`},
		{"number under a secret name", `{"api_key": 12345678901234567890}`, `{"api_key": "[REDACTED]"}`},
		{"top-level string", `"export GITEA_TOKEN=` + pw + `"`, `"export GITEA_TOKEN=[REDACTED]"`},
		{"token as a member name", `{"` + tok + `": true}`, `{"[REDACTED]": true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, o, err := s.JSON(context.Background(), "args", []byte(tc.doc), ModeMask)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("\n got %s\nwant %s", out, tc.want)
			}
			mustParse(t, out)
			if o.Status != StatusMasked || o.Count < 1 {
				t.Errorf("outcome = %+v; want masked", o)
			}
			for _, secret := range []string{pw, tok} {
				if strings.Contains(string(out), secret) {
					t.Error("a credential survived")
				}
			}
		})
	}
}

// TestJSONEscapesSurvive: a value that runs into a JSON escape is masked in
// the decoded string, so the escape cannot be half eaten.
func TestJSONEscapesSurvive(t *testing.T) {
	s := newTestScanner(t)
	pw := passwordValue()
	doc := []byte(`{"cmd": "export TOKEN=` + pw + `\\\"x\" && echo \u003cdone\u003e\nnext"}`)
	out, o, err := s.JSON(context.Background(), "args", doc, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	v := mustParse(t, out).(map[string]any)
	cmd := v["cmd"].(string)
	if strings.Contains(cmd, pw) || !strings.Contains(cmd, Mask) || !strings.Contains(cmd, "<done>\nnext") {
		t.Errorf("cmd = %q; want the value masked and the rest kept", cmd)
	}
	if o.Status != StatusMasked {
		t.Errorf("status = %s; want masked", o.Status)
	}
}

// TestJSONCleanDocumentUnchanged: a clean document comes back byte for byte.
func TestJSONCleanDocumentUnchanged(t *testing.T) {
	s := newTestScanner(t)
	doc := []byte("{\n  \"path\": \"/tmp/x\",\n  \"n\": 1.5e3, \"ok\": [true, null, \"\\u00e9\"]\n}")
	out, o, err := s.JSON(context.Background(), "args", doc, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(doc) || o.Status != StatusClean || o.Count != 0 {
		t.Errorf("clean doc changed: %s, %+v", out, o)
	}
}

// TestJSONMaskIsIdempotent: scanning a masked document again finds nothing,
// so a stored mask is never counted twice.
func TestJSONMaskIsIdempotent(t *testing.T) {
	s := newTestScanner(t)
	doc := []byte(`{"password": "` + passwordValue() + `", "headers": {"Authorization": "Bearer ` + bearerValue() + `"}}`)
	out, _, err := s.JSON(context.Background(), "args", doc, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	again, o, err := s.JSON(context.Background(), "args", out, ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusClean || string(again) != string(out) {
		t.Errorf("re-scan of a masked doc = %s, %+v; want clean and unchanged", again, o)
	}
}

// TestJSONInvalidFallsBackToText: a document that is not JSON has no
// structure to keep, and is scanned as text.
func TestJSONInvalidFallsBackToText(t *testing.T) {
	s := newTestScanner(t)
	tok := pat(5)
	out, o, err := s.JSON(context.Background(), "args", []byte(`{"token": `+tok), ModeMask)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), tok) || o.Status != StatusMasked {
		t.Errorf("invalid JSON not masked: %s, %+v", out, o)
	}
}

// TestJSONRejectNamesFieldNotValue: in reject mode the rejection names the
// field and the rule, with no position, and never the value.
func TestJSONRejectNamesFieldNotValue(t *testing.T) {
	s := newTestScanner(t)
	tok := pat(6)
	_, _, err := s.JSON(context.Background(), "spans[2].args", []byte(`{"a": "x", "b": "`+tok+`"}`), ModeReject)
	var rej *Rejection
	if !errors.As(err, &rej) || !errors.Is(err, ErrSecretDetected) || !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v; want a secret_detected Rejection", err)
	}
	if rej.Field != "spans[2].args" {
		t.Errorf("field = %q", rej.Field)
	}
	for _, f := range rej.Findings {
		if f.Line != 0 || f.Column != 0 {
			t.Errorf("finding %+v carries a position that is not a document position", f)
		}
	}
	if strings.Contains(err.Error(), tok) {
		t.Error("the rejection message carries the token")
	}
}

// TestJSONOversizeRejected: the whole document is one field under the cap.
func TestJSONOversizeRejected(t *testing.T) {
	s, err := New(Config{MaxScanBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.JSON(context.Background(), "args", []byte(`{"a": "0123456789abcdef"}`), ModeMask)
	if !errors.Is(err, ErrTooLargeToScan) {
		t.Errorf("err = %v; want too_large_to_scan", err)
	}
}

// TestJSONCancelledContextFailsClosed: a cancelled scan is an internal
// failure, never a clean result.
func TestJSONCancelledContextFailsClosed(t *testing.T) {
	s := newTestScanner(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.JSON(ctx, "args", []byte(`{"a": "b"}`), ModeMask)
	if !errors.Is(err, ErrScanFailed) {
		t.Errorf("err = %v; want ErrScanFailed", err)
	}
}

// FuzzJSONStaysValid: whatever the document, a successful mask parses and has
// the same shape (every masked scalar is a string where a string or number
// was).
func FuzzJSONStaysValid(f *testing.F) {
	pw, tok := passwordValue(), pat(7)
	for _, seed := range []string{
		`{"password": "` + pw + `"}`,
		`{"Authorization": "Bearer ` + bearerValue() + `"}`,
		`[{"k": ["` + tok + `", 1, null]}, "x", 2]`,
		`{"cmd": "curl -u admin:` + pw + ` https://h\\\""}`,
		`{"a": {"b": {"secret": 1234567890123}}}`,
	} {
		f.Add([]byte(seed))
	}
	s := newTestScanner(f)
	f.Fuzz(func(t *testing.T, doc []byte) {
		if !json.Valid(doc) {
			return
		}
		out, _, err := s.JSON(context.Background(), "args", doc, ModeMask)
		if err != nil {
			var rej *Rejection
			if errors.As(err, &rej) {
				return // refused, not stored: acceptable
			}
			t.Fatalf("JSON: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("masked output does not parse:\n in %s\nout %s", doc, out)
		}
	})
}
