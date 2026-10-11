package clicmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliexit"
)

// fakeCairn is the two routes `cairn read` and `cairn comment` touch, keyed by
// the bare public id: the metadata read and the body read. It records the paths
// it served so a test can prove which read a command chose.
type fakeCairn struct {
	*httptest.Server
	metadata  map[string]map[string]any
	bodies    map[string]string
	members   map[string]map[string]string
	comments  []string
	parentIDs []*int64
	paths     []string
}

func newFakeCairn(t *testing.T) *fakeCairn {
	t.Helper()
	f := &fakeCairn{
		metadata: map[string]map[string]any{},
		bodies:   map[string]string{},
		members:  map[string]map[string]string{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/body"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/artifacts/"), "/body")
			body, ok := f.bodies[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/members/"):
			rest := strings.TrimPrefix(r.URL.Path, "/v1/artifacts/")
			id, name, _ := strings.Cut(rest, "/members/")
			body, ok := f.members[id][name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/v1/artifacts/")
			meta, ok := f.metadata[id]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "not_found", "message": "artifact not found"},
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(meta)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				AnchorType string `json:"anchor_type"`
				ParentID   *int64 `json:"parent_id"`
				Body       string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode comment: %v", err)
			}
			if payload.AnchorType != "artifact" {
				t.Errorf("anchor_type = %q, want artifact", payload.AnchorType)
			}
			f.comments = append(f.comments, payload.Body)
			f.parentIDs = append(f.parentIDs, payload.ParentID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 12, "anchor_type": "artifact", "actor_id": "joe@stump.rocks", "body": payload.Body,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCairn) artifact(id string, meta map[string]any) {
	f.metadata[id] = meta
}

func TestReadPrintsBodyToStdout(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("abc123", map[string]any{"id": "abc123", "share_type": "markdown", "url": srv.URL + "/abc123"})
	srv.bodies["abc123"] = "# the body\n"

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "read", "abc123", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stdout)
	}
	if stdout != "# the body\n" {
		t.Fatalf("stdout = %q, want the bare body and nothing else", stdout)
	}
}

// The handle is what a Switchboard work order carries, so accepting it is the
// whole point of `read` for an agent.
func TestReadAcceptsMCPHandle(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("abc123", map[string]any{"id": "abc123", "share_type": "markdown", "url": srv.URL + "/abc123"})
	srv.bodies["abc123"] = "handle body"

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "read", "mcp://cairn/abc123", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout != "handle body" {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, p := range srv.paths {
		if strings.Contains(p, "mcp://") {
			t.Fatalf("handle reached the wire un-normalized: %v", srv.paths)
		}
	}
}

func TestReadBundleWithoutPathListsMembers(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("bnd1", map[string]any{
		"id": "bnd1", "share_type": "bundle", "url": srv.URL + "/bnd1",
		"members": []any{
			map[string]any{"name": "notes.md", "size": 12, "media_type": "text/markdown"},
			map[string]any{"name": "run.log", "size": 7, "media_type": "text/plain"},
		},
	})
	srv.bodies["bnd1"] = "should never be fetched"

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "read", "bnd1", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"2 files", "notes.md", "run.log", srv.URL + "/bnd1"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("member listing missing %q, got:\n%s", want, stdout)
		}
	}
	for _, p := range srv.paths {
		if strings.HasSuffix(p, "/body") {
			t.Fatalf("a bundle with no --path must not fetch a body: %v", srv.paths)
		}
	}
}

func TestReadBundleWithPathPrintsMember(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("bnd1", map[string]any{
		"id": "bnd1", "share_type": "bundle", "url": srv.URL + "/bnd1",
		"members": []any{map[string]any{"name": "notes.md", "size": 9, "media_type": "text/markdown"}},
	})
	srv.members["bnd1"] = map[string]string{"notes.md": "# member"}

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "read", "bnd1", "--path", "notes.md", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stdout != "# member" {
		t.Fatalf("stdout = %q, want the member's bytes", stdout)
	}
}

func TestReadJSONEmitsMetadataEnvelope(t *testing.T) {
	srv := newFakeCairn(t)
	srv.artifact("abc123", map[string]any{
		"id": "abc123", "share_type": "markdown", "url": srv.URL + "/abc123",
		"mcp": "mcp://cairn/abc123", "tags": []string{"handoff", "lane:m"}, "size": 5,
	})
	srv.bodies["abc123"] = "hello"

	stdout, errOut, code := runCLI(t, emptyConfigPath(t), "", "read", "abc123", "--json", "--url", srv.URL)
	if code != int(cliexit.Success) {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut)
	}
	var env readEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("--json stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if env.ShareType != "markdown" || env.MCP != "mcp://cairn/abc123" || env.Body != "hello" {
		t.Fatalf("envelope = %+v", env)
	}
	if env.BodyEncoding != "utf8" || env.BodyTruncated {
		t.Fatalf("body_encoding=%q truncated=%v, want utf8/false", env.BodyEncoding, env.BodyTruncated)
	}
	if len(env.Tags) != 2 || env.Tags[0] != "handoff" {
		t.Fatalf("tags = %v, want the routing tags to be visible to a script", env.Tags)
	}
}

func TestInlineBodyFlagsTruncation(t *testing.T) {
	restore := maxInlineReadBodyBytes
	maxInlineReadBodyBytes = 4
	t.Cleanup(func() { maxInlineReadBodyBytes = restore })

	env, err := inlineBody(readEnvelope{}, strings.NewReader("abcdef"))
	if err != nil {
		t.Fatalf("inlineBody: %v", err)
	}
	if env.Body != "abcd" || !env.BodyTruncated {
		t.Fatalf("body = %q truncated = %v, want a flagged 4-byte prefix", env.Body, env.BodyTruncated)
	}

	exact, err := inlineBody(readEnvelope{}, strings.NewReader("abcd"))
	if err != nil {
		t.Fatalf("inlineBody: %v", err)
	}
	if exact.Body != "abcd" || exact.BodyTruncated {
		t.Fatalf("body = %q truncated = %v, want the whole body and no flag", exact.Body, exact.BodyTruncated)
	}
}

func TestReadUnknownIDExitsNotFound(t *testing.T) {
	srv := newFakeCairn(t)

	stdout, _, code := runCLI(t, emptyConfigPath(t), "", "read", "nope", "--url", srv.URL)
	if code != int(cliexit.NotFound) {
		t.Fatalf("exit code = %d, want %d (stdout: %s)", code, cliexit.NotFound, stdout)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

func TestReadWithNoIDIsUsage(t *testing.T) {
	_, _, code := runCLI(t, emptyConfigPath(t), "", "read", "--url", "https://cairn.test")
	if code != int(cliexit.Usage) {
		t.Fatalf("exit code = %d, want %d", code, cliexit.Usage)
	}
}
