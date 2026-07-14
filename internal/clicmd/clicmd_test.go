package clicmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/joestump/cairn/internal/cliclient"
	"github.com/joestump/cairn/internal/cliconfig"
	"github.com/joestump/cairn/internal/cliexit"
)

// TestMain forces every test in this package onto zalando/go-keyring's
// in-memory mock, defaulted to "no secret service available." Without this,
// `cairn login` in these tests would reach whatever REAL OS keyring happens
// to be reachable on the machine running `go test` (this sandbox has a live
// one) and leave test credentials behind in it — this package's tests must
// be deterministic and side-effect-free regardless of the host, so they
// always exercise the SPEC-0008 "file fallback" path (internal/cliconfig
// has its own dedicated tests for the keyring-preferred branch).
func TestMain(m *testing.M) {
	keyring.MockInitWithError(errors.New("keyring: no secret service in test"))
	os.Exit(m.Run())
}

// whoamiTestServer stands up a minimal test double of the server's bearer
// auth: it accepts exactly one token as `wantToken` and resolves it to
// actor/channel; any other (or absent) bearer is a 401 in the ADR-0012
// error envelope shape, exactly like the real GET /v1/whoami.
func whoamiTestServer(t *testing.T, wantToken, actorID, channel string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		if got == "" || got != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "unauthorized", "message": "authentication required"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cliclient.Whoami{ActorID: actorID, Channel: channel, Authenticated: true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

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

func TestWhoamiWithValidTokenRoundTripsToServer(t *testing.T) {
	srv := whoamiTestServer(t, "abc123", "sam@stump.rocks", "via API")
	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "whoami", "--token", "abc123", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "sam@stump.rocks") {
		t.Errorf("stdout = %q, want it to mention the verified actor", stdout)
	}
	if !strings.Contains(stdout, "authorized") {
		t.Errorf("stdout = %q, want it to mention authorized", stdout)
	}
}

func TestWhoamiWithStaleTokenIsRejectedByServer(t *testing.T) {
	srv := whoamiTestServer(t, "the-real-token", "sam@stump.rocks", "via API")
	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "whoami", "--token", "a-revoked-token", "--url", srv.URL)
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.NotAuthenticated, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "not authenticated") {
		t.Errorf("stderr = %q, want it to mention not authenticated", stderr)
	}
	if strings.Contains(stderr, "a-revoked-token") {
		t.Errorf("stderr = %q, must never contain the rejected token value", stderr)
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

// TestBareIngestSendsTTLAndTitleFlags is SPEC-0008's `--ttl`/`--title` flags
// end to end: both reach the server as the corresponding headers on the
// single-artifact create request.
func TestBareIngestSendsTTLAndTitleFlags(t *testing.T) {
	var gotTTL, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTTL = r.Header.Get("X-Cairn-Ttl-Seconds")
		gotTitle = r.Header.Get("X-Cairn-Title")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	_, stderr, code := runCLI(t, emptyConfigPath(t), "hello world!", "--url", srv.URL, "--token", "tok",
		"--ttl", "24h", "--title", "my notes")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if gotTTL != "86400" {
		t.Errorf("X-Cairn-Ttl-Seconds = %q, want 86400", gotTTL)
	}
	if gotTitle != "my notes" {
		t.Errorf("X-Cairn-Title = %q, want %q", gotTitle, "my notes")
	}
}

// TestBareIngestInvalidTTLIsUsageError verifies --ttl parsing failures never
// reach the network (SPEC-0008 "usage error ... before any network call").
func TestBareIngestInvalidTTLIsUsageError(t *testing.T) {
	_, stderr, code := runCLI(t, emptyConfigPath(t), "hello", "--token", "tok", "--ttl", "not-a-duration")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.Usage, stderr)
	}
}

// TestBareIngestDetectsMarkdownByExtension is SPEC-0008's media-type
// detection: a .md path argument declares text/markdown without an explicit
// --type override.
func TestBareIngestDetectsMarkdownByExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("# hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "--url", srv.URL, "--token", "tok", path)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if gotContentType != "text/markdown" {
		t.Errorf("Content-Type = %q, want text/markdown", gotContentType)
	}
}

// TestBareIngestTypeFlagOverridesDetection verifies an explicit --type wins
// over extension-based detection.
func TestBareIngestTypeFlagOverridesDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("# hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "abc12", URL: "https://cairn.sh/abc12"})
	}))
	defer srv.Close()

	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "--url", srv.URL, "--token", "tok", "--type", "text/plain", path)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if gotContentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (explicit override)", gotContentType)
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

// TestAddSendsTTLAndTitleFlags is SPEC-0008 bundle creation's `--ttl`/
// `--title` flags end to end.
func TestAddSendsTTLAndTitleFlags(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	if err := os.WriteFile(a, []byte("log line"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var gotTTL, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTTL = r.Header.Get("X-Cairn-Ttl-Seconds")
		_ = r.ParseMultipartForm(1 << 20)
		gotTitle = r.MultipartForm.Value["title"][0]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(cliclient.Artifact{ID: "bundle1", URL: "https://cairn.sh/bundle1"})
	}))
	defer srv.Close()

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "add", a, "--url", srv.URL, "--token", "tok",
		"--ttl", "7d", "--title", "my bundle")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "https://cairn.sh/bundle1") {
		t.Errorf("stdout = %q, want the bundle link", stdout)
	}
	if want := "604800"; gotTTL != want {
		t.Errorf("X-Cairn-Ttl-Seconds = %q, want %q", gotTTL, want)
	}
	if gotTitle != "my bundle" {
		t.Errorf("title field = %q, want %q", gotTitle, "my bundle")
	}
}

// TestAddAbortsBundleWhenServerRejectsIt is SPEC-0008 "One file in the
// bundle fails": since the bundle is one atomic multipart POST, any server
// rejection aborts the whole bundle — the CLI MUST NOT print a success link
// and MUST exit non-zero.
func TestAddAbortsBundleWhenServerRejectsIt(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "payload_too_large", "message": "one member too big"},
		})
	}))
	defer srv.Close()

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "add", a, b, "--url", srv.URL, "--token", "tok")
	if code != int(cliexit.Oversize) {
		t.Fatalf("exit code = %d, want %d, stderr=%q", code, cliexit.Oversize, stderr)
	}
	if strings.Contains(stdout, "cairn.sh") {
		t.Errorf("stdout = %q, want no success link on a rejected bundle", stdout)
	}
}

// --- cairn login / logout (cairn#21) -----------------------------------

func TestLoginWithTokenFlagSucceedsAndPersists(t *testing.T) {
	srv := whoamiTestServer(t, "sk_live_login", "sam@stump.rocks", "via API")
	path := emptyConfigPath(t)

	stdout, stderr, code := runCLI(t, path, "", "login", "--token", "sk_live_login", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "sam@stump.rocks") {
		t.Errorf("stdout = %q, want it to mention the verified actor", stdout)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config file after login: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %s, want 0600", info.Mode().Perm())
	}

	// A follow-up command with NO flags at all resolves the same server and
	// token straight from what login just persisted.
	stdout, _, code = runCLI(t, path, "", "whoami")
	if code != int(cliexit.Success) {
		t.Fatalf("whoami after login exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "sam@stump.rocks") {
		t.Errorf("whoami stdout = %q, want the persisted actor", stdout)
	}
}

func TestLoginRejectsInvalidTokenAndPersistsNothing(t *testing.T) {
	srv := whoamiTestServer(t, "the-only-valid-token", "sam@stump.rocks", "via API")
	path := emptyConfigPath(t)

	_, stderr, code := runCLI(t, path, "", "login", "--token", "totally-invalid", "--url", srv.URL)
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.NotAuthenticated, stderr)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("config file was written for a rejected login")
	}

	cfg, err := cliconfig.Resolve(cliconfig.Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q after a rejected login, want empty", cfg.Token)
	}
}

func TestLoginReadsTokenFromPipedStdin(t *testing.T) {
	srv := whoamiTestServer(t, "piped-token", "sam@stump.rocks", "via API")
	path := emptyConfigPath(t)

	stdout, stderr, code := runCLI(t, path, "piped-token\n", "login", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "sam@stump.rocks") {
		t.Errorf("stdout = %q, want the verified actor", stdout)
	}
}

func TestLoginWithNoTokenSourceIsUsageError(t *testing.T) {
	// Stdin is a strings.Reader (never a terminal), so an empty pipe with no
	// --token is unambiguous "no token was provided" — a usage error, not a
	// hang waiting on a prompt that can't happen in a test harness.
	_, stderr, code := runCLI(t, emptyConfigPath(t), "", "login", "--url", "https://example.invalid")
	if code != int(cliexit.Usage) {
		t.Errorf("exit code = %d, want %d, stderr=%q", code, cliexit.Usage, stderr)
	}
}

func TestLoginThenLogoutRemovesCredential(t *testing.T) {
	srv := whoamiTestServer(t, "sk_live_logout", "sam@stump.rocks", "via API")
	path := emptyConfigPath(t)

	_, stderr, code := runCLI(t, path, "", "login", "--token", "sk_live_logout", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("login exit code = %d, want 0, stderr=%q", code, stderr)
	}

	stdout, _, code := runCLI(t, path, "", "logout")
	if code != int(cliexit.Success) {
		t.Fatalf("logout exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "logged out") {
		t.Errorf("logout stdout = %q, want a confirmation", stdout)
	}

	// whoami now has nothing to resolve — no --url either, since logout
	// doesn't touch the persisted URL, only the credential.
	_, stderr, code = runCLI(t, path, "", "whoami")
	if code != int(cliexit.NotAuthenticated) {
		t.Errorf("whoami after logout exit code = %d, want %d, stderr=%q", code, cliexit.NotAuthenticated, stderr)
	}
}

func TestLogoutWithNoActiveSessionIsIdempotent(t *testing.T) {
	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "logout")
	if code != int(cliexit.Success) {
		t.Errorf("exit code = %d, want 0 (logout is idempotent)", code)
	}
	if !strings.Contains(stdout, "not logged in") {
		t.Errorf("stdout = %q, want it to note there was nothing to remove", stdout)
	}
}

// TestLoginRedactsTokenEverywhere is cairn#21's "Token never appears in
// logs/errors/argv of subprocesses; redaction test": it drives login
// (success and failure) with --verbose, the one code path that
// deliberately prints diagnostic information about the token, and asserts
// the literal secret is never present in stdout or stderr — only its
// redacted form.
func TestLoginRedactsTokenEverywhere(t *testing.T) {
	const secret = "sk_live_SUPER_SECRET_DO_NOT_LEAK_1234567890"

	srv := whoamiTestServer(t, secret, "sam@stump.rocks", "via API")
	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "", "login", "--token", secret, "--url", srv.URL, "--verbose")
	if code != int(cliexit.Success) {
		t.Fatalf("login exit code = %d, want 0, stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, secret) {
		t.Errorf("stdout leaked the token: %q", stdout)
	}
	if strings.Contains(stderr, secret) {
		t.Errorf("stderr leaked the token: %q", stderr)
	}
	if !strings.Contains(stderr, "[REDACTED") {
		t.Errorf("stderr = %q, want the verbose line to show a redacted token marker", stderr)
	}

	// The failure path (server rejects the token) must redact it too, both
	// in the CLI's own message and in whatever the mapped error prints.
	stdout, stderr, code = runCLI(t, emptyConfigPath(t), "", "login", "--token", secret, "--url", srv.URL+"/wrong-path-forces-401", "--verbose")
	_ = stdout
	if code == int(cliexit.Success) {
		t.Fatalf("expected the login against a mismatched path to fail")
	}
	if strings.Contains(stderr, secret) {
		t.Errorf("stderr leaked the token on a failed login: %q", stderr)
	}
}

// --- Config-file credential precedence (cairn#21 extends the earlier
// SPEC-0008 config resolution test to a real login/whoami round trip) -----

func TestConfigFilePrecedenceThroughFullCommand(t *testing.T) {
	srv := whoamiTestServer(t, "from-file", "sam@stump.rocks", "via API")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := "url = \"" + srv.URL + "\"\ntoken = \"from-file\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Token and URL both come from the file; whoami should report
	// authenticated without any --token/--url flag or CAIRN_TOKEN/CAIRN_URL
	// env var, and must never print the token value itself — only its
	// source.
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
