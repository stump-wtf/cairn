package clicmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliexit"
)

func commentFixture(t *testing.T) *fakeCairn {
	t.Helper()
	srv := newFakeCairn(t)
	srv.artifact("abc123", map[string]any{
		"id": "abc123", "share_type": "markdown", "url": srv.URL + "/abc123",
	})
	return srv
}

func TestCommentArgumentBodyPrintsIDAndLink(t *testing.T) {
	srv := commentFixture(t)

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comment", "abc123", "looks good",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	if len(srv.comments) != 1 || srv.comments[0] != "looks good" {
		t.Fatalf("comments = %v, want the argument body", srv.comments)
	}
	for _, want := range []string{"#12", srv.URL + "/abc123"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q, got %q", want, stdout)
		}
	}
}

func TestCommentReadsPipedStdin(t *testing.T) {
	srv := commentFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "piped body\n", "comment", "abc123",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(srv.comments) != 1 || srv.comments[0] != "piped body\n" {
		t.Fatalf("comments = %v, want the piped body", srv.comments)
	}
}

func TestCommentDashReadsStdinExplicitly(t *testing.T) {
	srv := commentFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "explicit stdin", "comment", "abc123", "-",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(srv.comments) != 1 || srv.comments[0] != "explicit stdin" {
		t.Fatalf("comments = %v", srv.comments)
	}
}

func TestCommentFileFlag(t *testing.T) {
	srv := commentFixture(t)
	path := filepath.Join(t.TempDir(), "reply.md")
	if err := os.WriteFile(path, []byte("# from a file"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, code := runCLI(t, emptyConfigPath(t), "", "comment", "mcp://cairn/abc123", "--file", path,
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(srv.comments) != 1 || srv.comments[0] != "# from a file" {
		t.Fatalf("comments = %v, want the file contents", srv.comments)
	}
}

func TestCommentFileAndArgumentAreMutuallyExclusive(t *testing.T) {
	srv := commentFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "", "comment", "abc123", "one body", "--file", "/tmp/x.md",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Usage) {
		t.Fatalf("exit code = %d, want %d", code, cliexit.Usage)
	}
	if len(srv.comments) != 0 {
		t.Fatalf("comments = %v, want no request posted", srv.comments)
	}
}

func TestCommentParentReply(t *testing.T) {
	srv := commentFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "", "comment", "abc123", "replying", "--parent", "41",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if len(srv.parentIDs) != 1 || srv.parentIDs[0] == nil || *srv.parentIDs[0] != 41 {
		t.Fatalf("parent_ids = %v, want [41]", srv.parentIDs)
	}
}

func TestCommentWithoutSessionExitsNotAuthenticated(t *testing.T) {
	srv := commentFixture(t)

	stdout, stderr, code := runCLI(t, emptyConfigPath(t), "a body", "comment", "abc123", "--url", srv.URL)
	if code != int(cliexit.NotAuthenticated) {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, cliexit.NotAuthenticated, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if len(srv.paths) != 0 {
		t.Fatalf("requests = %v, want no request without a credential", srv.paths)
	}
}

func TestCommentEmptyBodyIsUsage(t *testing.T) {
	srv := commentFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "   \n", "comment", "abc123", "--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Usage) {
		t.Fatalf("exit code = %d, want %d", code, cliexit.Usage)
	}
	if len(srv.comments) != 0 {
		t.Fatalf("comments = %v, want nothing posted", srv.comments)
	}
}

func TestCommentJSONEnvelope(t *testing.T) {
	srv := commentFixture(t)

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comment", "abc123", "hi", "--json",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	var got commentResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if got.ID != 12 || got.ArtifactID != "abc123" || got.URL != srv.URL+"/abc123" {
		t.Fatalf("result = %+v", got)
	}
}

func TestCommentUnknownArtifactExitsNotFound(t *testing.T) {
	srv := newFakeCairn(t)

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "comment", "nope", "hi",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.NotFound) {
		t.Fatalf("exit code = %d, want %d (stdout: %s)", code, cliexit.NotFound, stdout)
	}
	if len(srv.comments) != 0 {
		t.Fatalf("comments = %v, want the write refused before posting", srv.comments)
	}
}
