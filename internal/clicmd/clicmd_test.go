package clicmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/cliclient"
	"github.com/joestump/cairn/internal/cliexit"
)

// runCLI builds a fresh root command against configPath, feeds stdin, and
// executes args, returning stdout, stderr, and the resulting exit code
// exactly as cmd/cairn/main.go would compute it — end-to-end coverage of
// the config → command → error → exit-code pipeline without exec-ing a
// binary (SPEC-0008 acceptance: "config precedence tested; exit codes
// stable").
func runCLI(t *testing.T, configPath string, stdin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	// Isolate from whatever CAIRN_URL/CAIRN_TOKEN happen to be set in the
	// ambient environment (e.g. a developer's shell) so precedence tests are
	// deterministic; individual tests override with t.Setenv as needed.
	t.Setenv("CAIRN_URL", "")
	t.Setenv("CAIRN_TOKEN", "")

	var out, errOut bytes.Buffer
	streams := IOStreams{In: strings.NewReader(stdin), Out: &out, ErrOut: &errOut}
	root := NewRootCmd(streams, configPath)
	root.SetArgs(args)

	err := root.ExecuteContext(context.Background())
	if err != nil {
		PrintError(&errOut, err, root)
	}
	return out.String(), errOut.String(), int(cliexit.ForError(err))
}

func emptyConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "config.toml")
}

func TestVersionFlag(t *testing.T) {
	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "--version")
	if code != int(cliexit.Success) {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "cairn version") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "cairn version")
	}
}

func TestHelpFlag(t *testing.T) {
	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "--help")
	if code != int(cliexit.Success) {
		t.Errorf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"cairn add", "cairn login", "cairn whoami", "cairn logout"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestWhoamiNoSessionExitsNotAuthenticated(t *testing.T) {
	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "whoami")
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("exit code = %d, want %d", code, cliexit.NotAuthenticated)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "not authenticated") {
		t.Errorf("stderr = %q, want it to mention not authenticated", stderr)
	}
}

func TestWhoamiWithTokenReportsAuthenticated(t *testing.T) {
	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "whoami", "--token", "abc123")
	if code != int(cliexit.Success) {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "authenticated") {
		t.Errorf("stdout = %q, want it to mention authenticated", stdout)
	}
}

func TestWhoamiJSONNoSession(t *testing.T) {
	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "whoami", "--json")
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("exit code = %d, want %d", code, cliexit.NotAuthenticated)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (no partial success object in --json mode)", stdout)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not valid JSON: %v (%q)", err, stderr)
	}
	if env.Error.Code != "unauthorized" {
		t.Errorf("error.code = %q, want unauthorized", env.Error.Code)
	}
}

func TestBareIngestNoAuthExitsNotAuthenticated(t *testing.T) {
	_, stderr, code := runCLI(t, emptyConfigPath(t), "hello world", "--url", "https://example.invalid")
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.NotAuthenticated, stderr)
	}
}

func TestBareIngestEmptyStdinIsUsageError(t *testing.T) {
	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "--token", "tok")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.Usage, stderr)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr = %q, want the usage tag", stderr)
	}
}

func TestBareIngestTooManyArgsIsUsageError(t *testing.T) {
	_, _, code := runCLI(t, emptyConfigPath(t), "", "--token", "tok", "a.txt", "b.txt")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d", code, cliexit.Usage)
	}
}

func TestBareIngestSuccessAgainstTestServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unauthorized", "message": "nope"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "abc12", URL: "https://cairn.sh/abc12", Size: 11})
	}))
	defer srv.Close()

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "hello world!", "--url", srv.URL, "--token", "tok")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "https://cairn.sh/abc12" {
		t.Errorf("stdout = %q, want the bare link", stdout)
	}
}

func TestBareIngestJSONErrorEnvelopeOnStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "payload_too_large", "message": "too big", "request_id": "req_9"},
		})
	}))
	defer srv.Close()

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "hello", "--url", srv.URL, "--token", "tok", "--json")
	if code != int(cliexit.Oversize) {
		t.Fatalf("exit code = %d, want %d", code, cliexit.Oversize)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (no partial success object in --json mode)", stdout)
	}
	var env struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not valid JSON: %v (%q)", err, stderr)
	}
	if env.Error.Code != "payload_too_large" {
		t.Errorf("error.code = %q", env.Error.Code)
	}
	if env.Error.RequestID != "req_9" {
		t.Errorf("error.request_id = %q", env.Error.RequestID)
	}
}

func TestAddDeduplicatesSamePathAndSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	if err := os.WriteFile(path, []byte("log line"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var fileCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		fileCount = len(r.MultipartForm.File["file"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1", Size: 8})
	}))
	defer srv.Close()

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "add", path, path, "--url", srv.URL, "--token", "tok")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if fileCount != 1 {
		t.Errorf("server saw %d file parts, want 1 (de-duplicated)", fileCount)
	}
	if !strings.Contains(stdout, "https://cairn.sh/bundle1") {
		t.Errorf("stdout = %q, want the bundle link", stdout)
	}
}

func TestAddMissingFileIsUsageError(t *testing.T) {
	_, _, code := runCLI(t, emptyConfigPath(t), "", "add", "/no/such/file", "--token", "tok")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d", code, cliexit.Usage)
	}
}

func TestLoginIsScaffoldedNotImplemented(t *testing.T) {
	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "login")
	if code != int(cliexit.Internal) {
		t.Errorf("exit code = %d, want %d", code, cliexit.Internal)
	}
	if !strings.Contains(stderr, "cairn#21") {
		t.Errorf("stderr = %q, want a pointer to cairn#21", stderr)
	}
}

func TestConfigFilePrecedenceThroughFullCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`token = "from-file"`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Token comes from the file; whoami should report authenticated without
	// any --token flag or CAIRN_TOKEN env var, and must never print the
	// token value itself — only its source.
	stdout, _, code := runCLI(t, path, "", "whoami")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "source: file") {
		t.Errorf("stdout = %q, want it to report source: file", stdout)
	}
	if strings.Contains(stdout, "from-file") {
		t.Errorf("stdout = %q, must never contain the token value", stdout)
	}
}

func TestMalformedURLFlagIsUsageError(t *testing.T) {
	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "whoami", "--url", "not-a-url", "--token", "x")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.Usage, stderr)
	}
}
