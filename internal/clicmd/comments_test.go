package clicmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliexit"
)

// commentsFixture is an artifact with the thread a project manager reads to
// decide whether a lane handed work back: a root, a reply, and a tombstone.
func commentsFixture(t *testing.T) *fakeCairn {
	t.Helper()
	srv := newFakeCairn(t)
	srv.artifact("abc123", map[string]any{
		"id": "abc123", "share_type": "markdown", "url": srv.URL + "/abc123", "comment_count": 3,
	})
	srv.commentThread("abc123",
		map[string]any{"id": 12, "anchor_type": "artifact", "actor_id": "joestump-agent",
			"actor_kind": "agent", "body": "HANDBACK: needs Joe (too big for one run)\nsecond line",
			"created_at": "2026-11-05T10:00:00Z"},
		map[string]any{"id": 13, "anchor_type": "artifact", "parent_id": 12, "actor_id": "joe@stump.rocks",
			"actor_kind": "human", "body": "agreed, splitting it", "created_at": "2026-11-05T11:00:00Z"},
		map[string]any{"id": 14, "anchor_type": "artifact", "actor_id": "joestump-agent",
			"actor_kind": "agent", "body": "", "deleted": true, "created_at": "2026-11-05T12:00:00Z"},
	)
	return srv
}

func TestCommentsJSONListsTheThreadInServerOrder(t *testing.T) {
	srv := commentsFixture(t)

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comments", "abc123", "--json",
		"--url", srv.URL, "--token", "pat")
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}

	var out struct {
		Comments []struct {
			ID       int64  `json:"id"`
			ParentID *int64 `json:"parent_id"`
			ActorID  string `json:"actor_id"`
			Body     string `json:"body"`
			Deleted  bool   `json:"deleted"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, stdout)
	}
	if len(out.Comments) != 3 {
		t.Fatalf("comments = %d, want the 3 the server listed", len(out.Comments))
	}
	if got := []int64{out.Comments[0].ID, out.Comments[1].ID, out.Comments[2].ID}; got[0] != 12 || got[1] != 13 || got[2] != 14 {
		t.Errorf("ids = %v, want the server's thread order kept", got)
	}
	if out.Comments[1].ParentID == nil || *out.Comments[1].ParentID != 12 {
		t.Errorf("reply parent_id = %v, want 12", out.Comments[1].ParentID)
	}
	if !out.Comments[2].Deleted {
		t.Error("tombstone lost its deleted flag on the way through the CLI")
	}
	if !strings.HasPrefix(out.Comments[0].Body, "HANDBACK: needs Joe") {
		t.Errorf("body = %q, want the hand-back text intact", out.Comments[0].Body)
	}
}

// The bug this command closes is a silent empty answer: `cairn read --json` has
// no comments key at all, so a hand-back check read "no comments" for every
// artifact. An artifact with nothing said on it must still answer with the key,
// so an empty thread is a finding and never a missing surface.
func TestCommentsJSONEmitsCommentsKeyWhenThreadIsEmpty(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("quiet", map[string]any{"id": "quiet", "share_type": "markdown", "url": srv.URL + "/quiet"})
	srv.commentThread("quiet")

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comments", "quiet", "--json", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, stdout)
	}
	raw, ok := out["comments"]
	if !ok {
		t.Fatalf("stdout has no comments key, which is indistinguishable from the missing surface: %s", stdout)
	}
	if string(raw) != "[]" {
		t.Errorf("comments = %s, want [] and never null", raw)
	}
}

func TestCommentsHumanListingShowsThreadShape(t *testing.T) {
	srv := commentsFixture(t)

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comments", "abc123", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{
		"3 comments",
		"#12 · joestump-agent",
		"HANDBACK: needs Joe (too big for one run)",
		"  second line",
		"#13 · joe@stump.rocks",
		"↳ reply to #12",
		"#14 · joestump-agent",
		" · deleted",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing missing %q, got:\n%s", want, stdout)
		}
	}
	if strings.Index(stdout, "#13") > strings.Index(stdout, "agreed, splitting it") {
		t.Errorf("a body printed before its own header row:\n%s", stdout)
	}
}

func TestCommentsHumanListingSaysSoWhenThereAreNone(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("quiet", map[string]any{"id": "quiet", "share_type": "markdown", "url": srv.URL + "/quiet"})
	srv.commentThread("quiet")

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "comments", "quiet", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "0 comments" {
		t.Fatalf("stdout = %q, want the count line and nothing else", stdout)
	}
}

// Reading comments is a link-capability read like `cairn read` (ADR-0007): a
// hand-back check runs on a box that never logged in, so requiring a credential
// here would put the thread back out of reach.
func TestCommentsNeedsNoCredential(t *testing.T) {
	srv := commentsFixture(t)

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "comments", "abc123", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 without a token (stdout: %s, stderr: %s)", code, stdout, errOut)
	}
}

func TestCommentsAcceptsMCPHandleAndFetchesOnlyComments(t *testing.T) {
	srv := commentsFixture(t)

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "comments", "mcp://cairn/abc123", "--json", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stdout: %s)", code, stdout)
	}
	want := "GET /v1/artifacts/abc123/comments"
	if len(srv.paths) != 1 || srv.paths[0] != want {
		t.Fatalf("requests = %v, want exactly [%s]", srv.paths, want)
	}
}

func TestCommentsUnknownArtifactExitsNotFound(t *testing.T) {
	srv := commentsFixture(t)

	_, errOut, code := runCLI(t, emptyConfigPath(t), "", "comments", "nope99", "--json", "--url", srv.URL)
	if code != int(cliexit.NotFound) {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, cliexit.NotFound, errOut)
	}
}

func TestCommentsArgumentCountIsUsageError(t *testing.T) {
	for _, args := range [][]string{{}, {"abc123", "extra"}} {
		_, _, code := runCLI(t, emptyConfigPath(t), "", append([]string{"comments"}, args...)...)
		if code != int(cliexit.Usage) {
			t.Errorf("args %v: exit code = %d, want %d", args, code, cliexit.Usage)
		}
	}
}

func TestCommentsWithoutAnIDInArgumentIsUsageError(t *testing.T) {
	srv := commentsFixture(t)

	_, _, code := runCLI(t, emptyConfigPath(t), "", "comments", "", "--url", srv.URL)
	if code != int(cliexit.Usage) {
		t.Fatalf("exit code = %d, want %d", code, cliexit.Usage)
	}
	if len(srv.paths) != 0 {
		t.Errorf("an argument with no id in it must fail before any request, got %v", srv.paths)
	}
}
