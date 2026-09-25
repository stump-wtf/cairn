package clicmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliexit"
)

// Governing: ADR-0025, SPEC-0019 VE-8, VE-9, SPEC-0008 REQ "Machine-Readable
// Error Mapping and Exit Codes"

// The fixtures below are the server's wire shape, written out by hand: the
// CLI never imports the server's packages (ADR-0003), and neither do its
// tests.

// rejectWith is a create endpoint that records the request's TTL and tag
// headers and answers with a validation_failed envelope carrying violations
// (nil for a server that predates them).
func rejectWith(t *testing.T, violations []map[string]any, gotTTL, gotTags *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotTTL != nil {
			*gotTTL = r.Header.Get("X-Cairn-Ttl-Seconds")
		}
		if gotTags != nil {
			*gotTags = r.Header.Get("X-Cairn-Tags")
		}
		body := map[string]any{"code": "validation_failed", "message": "the request was invalid"}
		if violations != nil {
			body["violations"] = violations
			body["message"] = "summary from the server"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// ttlOverCap is what a 30-day server says to X-Cairn-Ttl-Seconds: 5184000
// (SPEC-0019 VE-1 scenario "TTL over the cap").
var ttlOverCap = map[string]any{
	"field": "X-Cairn-Ttl-Seconds", "location": "header", "reason": "exceeds_max",
	"limit": 2592000, "unit": "seconds", "value": "5184000",
	"message": `"5184000" exceeds the maximum of 2592000 seconds`,
}

// TestTTLOverCapNamesTheFlag is SPEC-0019 VE-8's scenario, on both create
// commands: the exact line, and the usage exit code.
func TestTTLOverCapNamesTheFlag(t *testing.T) {
	notes := writeTemp(t, "notes.md", "# notes")
	for name, args := range map[string][]string{
		"add":    {"add", notes},
		"ingest": {notes},
	} {
		t.Run(name, func(t *testing.T) {
			var gotTTL string
			srv := rejectWith(t, []map[string]any{ttlOverCap}, &gotTTL, nil)
			args := append(args, "--ttl", "60d", "--url", srv.URL, "--token", "tok")
			stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", args...)

			if gotTTL != "5184000" {
				t.Errorf("X-Cairn-Ttl-Seconds = %q, want 5184000", gotTTL)
			}
			if want := "cairn: --ttl 60d exceeds the server's maximum of 30d\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if code != int(cliexit.Usage) {
				t.Errorf("exit code = %d, want usage (%d)", code, cliexit.Usage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

// TestUppercaseTagFoldedWithWarning is SPEC-0019 VE-9 "Uppercase tag folded
// with a warning": the request carries size:m, stderr warns, the create
// succeeds, and --json stdout holds only the artifact.
func TestUppercaseTagFoldedWithWarning(t *testing.T) {
	plan := writeTemp(t, "plan.md", "# plan")
	for name, extra := range map[string][]string{"human": nil, "json": {"--json"}} {
		t.Run(name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Cairn-Tags")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "b1", URL: "https://cairn.sh/b1"})
			}))
			defer srv.Close()

			args := append([]string{"add", plan, "--tag", "size:M", "--url", srv.URL, "--token", "tok"}, extra...)
			stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", args...)
			if code != int(cliexit.Success) {
				t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
			}
			if got != "size:m" {
				t.Errorf("X-Cairn-Tags = %q, want size:m", got)
			}
			if want := "cairn: warning: tag \"size:M\" sent as \"size:m\"\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if strings.Contains(stdout, "warning") {
				t.Errorf("stdout = %q, want no warning on stdout", stdout)
			}
			if extra != nil {
				var art cliclient.Artifact
				if err := json.Unmarshal([]byte(stdout), &art); err != nil || art.ID != "b1" {
					t.Errorf("--json stdout = %q is not the artifact (%v)", stdout, err)
				}
			}
		})
	}
}

// TestCharsetStillTheServersCall is SPEC-0019 VE-9 "Charset still the
// server's call": "lane m" goes out unchanged, and the server's
// invalid_charset violation comes back as the --tag line.
func TestCharsetStillTheServersCall(t *testing.T) {
	var got string
	srv := rejectWith(t, []map[string]any{{
		"field": "tag", "location": "header", "reason": "invalid_charset", "value": "lane m",
		"message": `"lane m" contains a character that is not allowed: use only lowercase a-z, 0-9 and ._:/#-`,
	}}, nil, &got)

	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "add", writeTemp(t, "plan.md", "x"),
		"--tag", "lane m", "--url", srv.URL, "--token", "tok")
	if got != "lane m" {
		t.Errorf("X-Cairn-Tags = %q, want %q unchanged", got, "lane m")
	}
	want := "cairn: --tag \"lane m\" contains a character that is not allowed: use only lowercase a-z, 0-9 and ._:/#-\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want usage (%d)", code, cliexit.Usage)
	}
}

// TestOldServerShowsTopLevelMessage: a server with no violations, or with
// only the generic one an unmigrated validator produces, renders as before.
func TestOldServerShowsTopLevelMessage(t *testing.T) {
	generic := map[string]any{"field": "request", "location": "body", "reason": "invalid", "message": "the request was invalid"}
	for name, vs := range map[string][]map[string]any{
		"no violations":     nil,
		"generic violation": {generic},
	} {
		t.Run(name, func(t *testing.T) {
			srv := rejectWith(t, vs, nil, nil)
			_, stderr, code := runCLI(t, emptyConfigPath(t), "body", "--url", srv.URL, "--token", "tok")
			msg := "the request was invalid"
			if vs != nil {
				msg = "summary from the server"
			}
			if want := "cairn: usage: " + msg + "\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if code != int(cliexit.Usage) {
				t.Errorf("exit code = %d, want usage (%d)", code, cliexit.Usage)
			}
		})
	}
}

// TestJSONErrorCarriesViolations: --json passes the server's violations
// through on stderr, limit and all, rather than the rendered lines.
func TestJSONErrorCarriesViolations(t *testing.T) {
	srv := rejectWith(t, []map[string]any{ttlOverCap}, nil, nil)
	stdout, stderr, _ := runCLI(t, emptyConfigPath(t), "body", "--ttl", "60d", "--json", "--url", srv.URL, "--token", "tok")
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	var env struct {
		Error struct {
			Code       string           `json:"code"`
			Violations []map[string]any `json:"violations"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not JSON: %v (%q)", err, stderr)
	}
	if env.Error.Code != "validation_failed" || len(env.Error.Violations) != 1 {
		t.Fatalf("error = %+v", env.Error)
	}
	v := env.Error.Violations[0]
	if v["field"] != "X-Cairn-Ttl-Seconds" || v["limit"] != float64(2592000) || v["value"] != "5184000" {
		t.Errorf("violation = %v", v)
	}
}

// TestViolationLines covers the field-to-flag table (SPEC-0019 design, CLI)
// and the limit formats, one line per violation.
func TestViolationLines(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(s string) *cliclient.Scalar { return &cliclient.Scalar{Text: s, Number: true} }
	sent := sentRequest{ttl: "60d", tags: []string{"handoff", "size:m", "x"}, files: []string{"a.log", "my notes.md"}}

	tests := []struct {
		name string
		v    cliclient.Violation
		want string
	}{
		{"policy ttl field", cliclient.Violation{Field: "ttl_seconds", Reason: "exceeds_max", Limit: num("2592000"), Unit: "seconds", Value: str("5184000")},
			"--ttl 60d exceeds the server's maximum of 30d"},
		{"ttl, server sentence", cliclient.Violation{Field: "X-Cairn-Ttl-Seconds", Reason: "not_positive", Value: str("0"),
			Message: `"0" is not valid: it must be a positive integer number of seconds`},
			"--ttl 60d is not valid: it must be a positive integer number of seconds"},
		{"indexed tag, value echoed", cliclient.Violation{Field: "tags[1]", Reason: "uppercase", Value: str("Size:M"), Message: `"Size:M" must be lowercase`},
			"--tag Size:M must be lowercase"},
		{"indexed tag, value from the request", cliclient.Violation{Field: "tags[2]", Reason: "too_long", Limit: num("64"), Unit: "bytes"},
			"--tag x is too long: the server's maximum is 64 bytes"},
		{"header tag list", cliclient.Violation{Field: "X-Cairn-Tags", Reason: "too_many", Limit: num("16"), Unit: "count"},
			"--tag has too many entries: the server's maximum is 16"},
		{"type header", cliclient.Violation{Field: "X-Cairn-Type", Reason: "unknown_value", Value: str("gist"), Message: `"gist" is not a recognised value`},
			"--type gist is not a recognised value"},
		{"type query", cliclient.Violation{Field: "type", Reason: "required", Message: "is required"},
			"--type is required"},
		{"title in chars", cliclient.Violation{Field: "X-Cairn-Title", Reason: "too_long", Limit: num("200"), Unit: "chars", Value: str("long")},
			"--title long is too long: the server's maximum is 200 chars"},
		{"redaction header", cliclient.Violation{Field: "X-Cairn-Redaction", Reason: "unknown_value", Value: str("scrub"), Message: `"scrub" is not a recognised value`},
			"--redact scrub is not a recognised value"},
		{"member size", cliclient.Violation{Field: "members[1].size", Reason: "too_large", Limit: num("10485760"), Unit: "bytes"},
			`"my notes.md" is too large: the server's maximum is 10 MiB`},
		{"member secret", cliclient.Violation{Field: "members[0].content", Reason: "secret_detected", Rule: "generic-api-key", Line: num("3"),
			Message: "a credential was detected (rule generic-api-key, line 3); remove it or resend with --redact=mask"},
			"a.log: a credential was detected (rule generic-api-key, line 3); remove it or resend with --redact=mask"},
		{"member out of range", cliclient.Violation{Field: "members[9].name", Reason: "duplicate", Message: "is a duplicate"},
			"members[9].name is a duplicate"},
		{"uneven byte cap", cliclient.Violation{Field: "body", Reason: "too_large", Limit: num("10000000"), Unit: "bytes"},
			"body is too large: the server's maximum is 10000000 bytes"},
		{"limit as a string", cliclient.Violation{Field: "X-Cairn-Ttl-Seconds", Reason: "exceeds_max", Limit: &cliclient.Scalar{Text: "86400"}, Unit: "seconds"},
			"--ttl 60d exceeds the server's maximum of 1d"},
		{"unknown field", cliclient.Violation{Field: "X-Cairn-Sha256", Reason: "checksum_mismatch", Message: "does not match the SHA-256 of the received body"},
			"X-Cairn-Sha256 does not match the SHA-256 of the received body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := violationLines([]cliclient.Violation{tc.v}, sent)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("violationLines = %q, want [%q]", got, tc.want)
			}
		})
	}
}

// TestViolationLinesOnePerViolation: every violation gets its own line, in
// order (SPEC-0019 VE-4, VE-8).
func TestViolationLinesOnePerViolation(t *testing.T) {
	str := func(s string) *string { return &s }
	vs := []cliclient.Violation{
		{Field: "tags[0]", Reason: "uppercase", Value: str("Size:M"), Message: `"Size:M" must be lowercase`},
		{Field: "tags[1]", Reason: "invalid_charset", Value: str("lane m"), Message: `"lane m" contains a character that is not allowed`},
	}
	vs = append(vs, cliclient.Violation{Field: "X-Cairn-Ttl-Seconds", Reason: "exceeds_max",
		Limit: &cliclient.Scalar{Text: "2592000", Number: true}, Unit: "seconds", Value: str("5184000")})
	got := violationLines(vs, sentRequest{})
	want := []string{
		"--tag Size:M must be lowercase",
		`--tag "lane m" contains a character that is not allowed`,
		// Without the typed --ttl, the echoed seconds are shown as a duration.
		"--ttl 60d exceeds the server's maximum of 30d",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("violationLines = %q, want %q", got, want)
	}
}
