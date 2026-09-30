package clicmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliexit"
)

// Governing: ADR-0023, SPEC-0017 RD-5, RD-9, RD-10, RD-11

// The fixtures below are the server's wire shape, written out by hand: the
// CLI never imports the server's packages (ADR-0003), and neither do its
// tests. None of them holds a credential; the server never sends one.

// createdWith is a create endpoint that counts requests, records each one's
// X-Cairn-Redaction header, and answers 201 with outcome merged into the
// artifact.
func createdWith(t *testing.T, outcome map[string]any, hits *atomic.Int32, gotRedaction *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*gotRedaction = r.Header.Get("X-Cairn-Redaction")
		art := map[string]any{"id": "abc12", "url": "https://cairn.sh/abc12", "size": 40}
		for k, v := range outcome {
			art[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(art)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// maskedTwice is the outcome of a create whose body held two GitHub tokens.
var maskedTwice = map[string]any{
	"redaction_status": "masked", "redacted": true,
	"redactions": map[string]any{"count": 2, "rules": map[string]any{"github-pat": 2}},
}

// TestRedactFlagSendsHeader is SPEC-0017 RD-5 in the CLI: --redact=mask
// becomes X-Cairn-Redaction: mask on both create commands, and without the
// flag no header is sent, so the server's default mode applies.
func TestRedactFlagSendsHeader(t *testing.T) {
	patch := writeTemp(t, "patch.diff", "+ a line\n")
	for name, args := range map[string][]string{
		"add":    {"add", patch},
		"ingest": {patch},
	} {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			var got string
			srv := createdWith(t, nil, &hits, &got)

			flagged := append(append([]string{}, args...), "--redact=mask", "--url", srv.URL, "--token", "tok")
			if _, stderr, code := runCLI(t, emptyConfigPath(t), "", flagged...); code != int(cliexit.Success) {
				t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
			}
			if got != "mask" {
				t.Errorf("X-Cairn-Redaction = %q with --redact=mask, want mask", got)
			}

			plain := append(append([]string{}, args...), "--url", srv.URL, "--token", "tok")
			if _, stderr, code := runCLI(t, emptyConfigPath(t), "", plain...); code != int(cliexit.Success) {
				t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
			}
			if got != "" {
				t.Errorf("X-Cairn-Redaction = %q without --redact, want no header", got)
			}
		})
	}
}

// TestRedactFlagInvalidValueIsUsageError: any value but mask, "off" and an
// explicit empty one included, is a usage error that never reaches the
// server (SPEC-0017 RD-5 "Disable attempt refused", SPEC-0008 usage errors
// before any network call).
func TestRedactFlagInvalidValueIsUsageError(t *testing.T) {
	patch := writeTemp(t, "patch.diff", "+ a line\n")
	for _, value := range []string{"off", "MASK", "reject", ""} {
		for name, args := range map[string][]string{
			"add":    {"add", patch},
			"ingest": {patch},
		} {
			t.Run(name+"/"+value, func(t *testing.T) {
				var hits atomic.Int32
				var got string
				srv := createdWith(t, nil, &hits, &got)
				args := append(append([]string{}, args...), "--redact="+value, "--url", srv.URL, "--token", "tok")
				stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", args...)

				if code != int(cliexit.Usage) {
					t.Errorf("exit code = %d, want usage (%d)", code, cliexit.Usage)
				}
				if n := hits.Load(); n != 0 {
					t.Errorf("server saw %d requests, want none", n)
				}
				if !strings.Contains(stderr, "--redact") || !strings.Contains(stderr, "the only value is mask") {
					t.Errorf("stderr = %q, want it to name --redact and its one value", stderr)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
			})
		}
	}
}

// TestMaskedCreateWarnsOnStderr is SPEC-0017 RD-9/RD-10 in the CLI: a
// response with redacted: true prints one warning on stderr, and stdout
// carries only what it always does, the bare link or the --json artifact.
func TestMaskedCreateWarnsOnStderr(t *testing.T) {
	const warning = "cairn: warning: 2 values masked (github-pat ×2); stored content differs from input\n"
	notes := writeTemp(t, "notes.md", "# notes\n")
	for name, args := range map[string][]string{
		"add":         {"add", notes},
		"ingest":      {notes},
		"add json":    {"add", notes, "--json"},
		"ingest json": {notes, "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			var got string
			srv := createdWith(t, maskedTwice, &hits, &got)
			args := append(append([]string{}, args...), "--url", srv.URL, "--token", "tok")
			stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", args...)

			if code != int(cliexit.Success) {
				t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
			}
			if stderr != warning {
				t.Errorf("stderr = %q, want %q", stderr, warning)
			}
			if strings.Contains(stdout, "warning") {
				t.Errorf("stdout = %q, want no warning in it", stdout)
			}
			if strings.HasSuffix(name, "json") {
				var art cliclient.Artifact
				dec := json.NewDecoder(strings.NewReader(stdout))
				if err := dec.Decode(&art); err != nil || dec.More() {
					t.Fatalf("stdout %q is not exactly one JSON artifact: %v", stdout, err)
				}
				if !art.WasRedacted() || art.Redactions == nil || art.Redactions.Count != 2 {
					t.Errorf("--json artifact = %+v, want the outcome passed through", art)
				}
			} else if !strings.HasSuffix(stdout, "https://cairn.sh/abc12\n") {
				t.Errorf("stdout = %q, want it to end with the link", stdout)
			}
		})
	}
}

// TestCleanCreateDoesNotWarn: a clean create, and a server that predates
// scanning, print no warning.
func TestCleanCreateDoesNotWarn(t *testing.T) {
	notes := writeTemp(t, "notes.md", "# notes\n")
	for name, outcome := range map[string]map[string]any{
		"clean": {"redaction_status": "clean", "redacted": false,
			"redactions": map[string]any{"count": 0, "rules": map[string]any{}}},
		"old server": nil,
	} {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			var got string
			srv := createdWith(t, outcome, &hits, &got)
			_, stderr, code := runCLI(t, emptyConfigPath(t), "", notes, "--url", srv.URL, "--token", "tok")
			if code != int(cliexit.Success) {
				t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
		})
	}
}

// TestWarnRedacted covers the warning's wording: rules sorted by ID, the
// singular, and a masked response with no counts.
func TestWarnRedacted(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name string
		art  *cliclient.Artifact
		want string
	}{
		{"two rules, sorted", &cliclient.Artifact{Redacted: &yes, Redactions: &cliclient.Redactions{
			Count: 3, Rules: map[string]int{"github-pat": 2, "aws-access-token": 1}}},
			"cairn: warning: 3 values masked (aws-access-token ×1, github-pat ×2); stored content differs from input\n"},
		{"one value", &cliclient.Artifact{Redacted: &yes, Redactions: &cliclient.Redactions{
			Count: 1, Rules: map[string]int{"slack-bot-token": 1}}},
			"cairn: warning: 1 value masked (slack-bot-token ×1); stored content differs from input\n"},
		{"no counts", &cliclient.Artifact{Redacted: &yes},
			"cairn: warning: values masked; stored content differs from input\n"},
	} {
		var buf bytes.Buffer
		warnRedacted(&buf, tc.art)
		if buf.String() != tc.want {
			t.Errorf("%s: warnRedacted = %q, want %q", tc.name, buf.String(), tc.want)
		}
	}
}

// secretAt is the violation the server sends for a credential in field on
// line 42: the rule, line and column, and never the value (SPEC-0017 RD-11).
func secretAt(field string) map[string]any {
	return map[string]any{
		"field": field, "location": "body", "reason": "secret_detected",
		"rule": "github-pat", "line": 42, "column": 7,
		"message": "a credential was detected (rule github-pat, line 42); remove it or resend with --redact=mask",
	}
}

// TestSecretRejectionShowsHowToProceed is SPEC-0017 RD-11 "CLI shows how to
// proceed": a create refused for a token on line 42 prints the file, the
// line, the rule ID and the --redact=mask hint, one line per violation, and
// exits with the usage code. A single-file create's field is body; a
// bundle's is members[n].content, the n-th file argument.
func TestSecretRejectionShowsHowToProceed(t *testing.T) {
	patch := writeTemp(t, "patch.diff", "+ a line\n")
	notes := writeTemp(t, "notes.md", "# notes\n")
	const hint = ": credential detected (rule github-pat); remove it or resend with --redact=mask\n"
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		field string
		want  string
	}{
		{"add, one file", []string{"add", patch}, "", "body", "cairn: " + shellWord(patch) + " line 42" + hint},
		{"ingest a file", []string{patch}, "", "body", "cairn: " + shellWord(patch) + " line 42" + hint},
		{"ingest stdin", nil, "+ a line\n", "body", "cairn: stdin line 42" + hint},
		{"add, a bundle member", []string{"add", notes, patch}, "", "members[1].content", "cairn: " + shellWord(patch) + " line 42" + hint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := rejectWith(t, []map[string]any{secretAt(tc.field)}, nil, nil)
			args := append(append([]string{}, tc.args...), "--url", srv.URL, "--token", "tok")
			stdout, stderr, code := runCLI(t, emptyConfigPath(t), tc.stdin, args...)

			if stderr != tc.want {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
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

// TestScanRejectionLines covers the rest of the scan rejections' wording:
// several findings, one line each; a value the server could not mask under
// --redact=mask, where resending with it would not help; a detection with no
// line; and a body too large to scan.
func TestScanRejectionLines(t *testing.T) {
	num := func(s string) *cliclient.Scalar { return &cliclient.Scalar{Text: s, Number: true} }
	files := sentRequest{files: []string{"a.log", "my patch.diff"}}
	for _, tc := range []struct {
		name string
		vs   []cliclient.Violation
		sent sentRequest
		want []string
	}{
		{"two findings in two members", []cliclient.Violation{
			{Field: "members[0].content", Reason: "secret_detected", Rule: "aws-access-token", Line: num("3")},
			{Field: "members[1].content", Reason: "secret_detected", Rule: "github-pat", Line: num("42")},
		}, files, []string{
			"a.log line 3: credential detected (rule aws-access-token); remove it or resend with --redact=mask",
			`"my patch.diff" line 42: credential detected (rule github-pat); remove it or resend with --redact=mask`,
		}},
		{"already masking", []cliclient.Violation{
			{Field: "body", Reason: "secret_detected", Rule: "github-pat", Line: num("42")},
		}, sentRequest{files: []string{"patch.diff"}, redact: "mask"}, []string{
			"patch.diff line 42: credential detected (rule github-pat); remove it",
		}},
		{"no line", []cliclient.Violation{
			{Field: "title", Reason: "secret_detected", Rule: "github-pat"},
		}, sentRequest{}, []string{
			"--title: credential detected (rule github-pat); remove it or resend with --redact=mask",
		}},
		{"member too large to scan", []cliclient.Violation{
			{Field: "members[1].content", Reason: "too_large_to_scan", Limit: num("1048576"), Unit: "bytes",
				Message: "is too large to scan for credentials: the maximum is 1048576 bytes"},
		}, files, []string{
			`"my patch.diff" is too large to scan for credentials: the server's maximum is 1 MiB`,
		}},
		{"stdin too large to scan, no limit", []cliclient.Violation{
			{Field: "body", Reason: "too_large_to_scan"},
		}, sentRequest{}, []string{
			"stdin is too large to scan for credentials",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := violationLines(tc.vs, tc.sent)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("violationLines = %q, want %q", got, tc.want)
			}
		})
	}
}
