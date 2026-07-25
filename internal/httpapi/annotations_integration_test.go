package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/store"
)

// jsonReader marshals v to a JSON body reader for a request.
func jsonReader(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(b)
}

// createArtifact seeds an artifact of the given share type over the API and
// returns its public id.
func createArtifact(t *testing.T, srvURL, shareType, actor, body string) string {
	t.Helper()
	resp := do(t, http.MethodPost, srvURL+"/v1/artifacts?type="+shareType, actor,
		strings.NewReader(body), "text/plain")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed %s artifact: status = %d, want 201", shareType, resp.StatusCode)
	}
	return decodeArtifact(t, resp).ID
}

func decodeReaction(t *testing.T, resp *http.Response) reactionResponse {
	t.Helper()
	defer resp.Body.Close()
	var r reactionResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode reaction: %v", err)
	}
	return r
}

func decodeTallies(t *testing.T, resp *http.Response) tallyResponse {
	t.Helper()
	defer resp.Body.Close()
	var tr tallyResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode tallies: %v", err)
	}
	return tr
}

func decodeComments(t *testing.T, resp *http.Response) commentsResponse {
	t.Helper()
	defer resp.Body.Close()
	var cr commentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	return cr
}

func reactionCount(t *testing.T, srvURL, id string) int {
	t.Helper()
	resp := do(t, http.MethodGet, srvURL+"/v1/artifacts/"+id, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get artifact %s: status %d", id, resp.StatusCode)
	}
	return decodeArtifact(t, resp).ReactionCount
}

func commentCount(t *testing.T, srvURL, id string) int {
	t.Helper()
	resp := do(t, http.MethodGet, srvURL+"/v1/artifacts/"+id, "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get artifact %s: status %d", id, resp.StatusCode)
	}
	return decodeArtifact(t, resp).CommentCount
}

// TestIntegrationAnnotationAuthAndUniform404 covers the auth-by-default and
// uniform-404 web-facing guarantees: an unauthenticated write is 401, and an
// authenticated write or read against an unknown artifact is an indistinguishable
// 404 (SPEC-0006 REQ "Authentication & Authorization", SPEC-0002 uniform 404).
func TestIntegrationAnnotationAuthAndUniform404(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// Unauthenticated react → 401, nothing persisted.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/zzzzzzzz/reactions", "",
		jsonReader(t, reactionRequest{AnchorType: "artifact", Emoji: "🔥"}), "application/json")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth react = %d, want 401", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", env.Error.Code)
	}

	// Authenticated react against an unknown id → uniform 404.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/zzzzzzzz/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "artifact", Emoji: "🔥"}), "application/json")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("react unknown id = %d, want 404", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", env.Error.Code)
	}

	// Reading annotations of an unknown id → same uniform 404.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/zzzzzzzz/comments", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list comments unknown id = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/zzzzzzzz/reactions", "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list reactions unknown id = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationReactionToggleIdempotent drives the idempotent toggle over
// REST: react, react-again-is-a-no-op, the per-anchor tally with the actor's
// "reacted" flag, the artifact rollup, then un-react (twice) back to zero
// (SPEC-0006 REQ "Idempotent Reactions", REQ "Count Aggregation").
func TestIntegrationReactionToggleIdempotent(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "code", "alice", "package main")

	body := reactionRequest{AnchorType: "artifact", Emoji: "🔥"}

	// First react → 201.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, body), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first react = %d, want 201", resp.StatusCode)
	}
	first := decodeReaction(t, resp)

	// Duplicate react → 200 no-op, same row id.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, body), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate react = %d, want 200 (no-op)", resp.StatusCode)
	}
	if dup := decodeReaction(t, resp); dup.ID != first.ID {
		t.Fatalf("duplicate react returned id %d, want existing %d", dup.ID, first.ID)
	}

	// Tally: one 🔥, and the authenticated reader sees their own reacted=true.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list reactions = %d, want 200", resp.StatusCode)
	}
	tallies := decodeTallies(t, resp)
	if len(tallies.Reactions) != 1 {
		t.Fatalf("tallies = %d entries, want 1", len(tallies.Reactions))
	}
	if got := tallies.Reactions[0]; got.Emoji != "🔥" || got.Count != 1 || !got.Reacted || got.AnchorType != "artifact" {
		t.Fatalf("tally = %+v, want 🔥 count 1 reacted on artifact", got)
	}
	if n := reactionCount(t, srv.URL, id); n != 1 {
		t.Fatalf("artifact reaction_count = %d, want 1", n)
	}

	// An anonymous reader sees the count but not a reacted flag.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "", nil, "")
	if anon := decodeTallies(t, resp); len(anon.Reactions) != 1 || anon.Reactions[0].Reacted {
		t.Fatalf("anonymous tally = %+v, want count 1 reacted=false", anon.Reactions)
	}

	// Un-react by value → 204, tally empties, rollup returns to 0.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, body), "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("un-react = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Un-react again is an idempotent no-op → still 204.
	resp = do(t, http.MethodDelete, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, body), "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("repeat un-react = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice", nil, "")
	if empty := decodeTallies(t, resp); len(empty.Reactions) != 0 {
		t.Fatalf("tallies after un-react = %d, want 0", len(empty.Reactions))
	}
	if n := reactionCount(t, srv.URL, id); n != 0 {
		t.Fatalf("artifact reaction_count after un-react = %d, want 0", n)
	}
}

// TestIntegrationUnreactByIDAuthorOnly covers the ADR-0012 DELETE
// /reactions/{rid} shape: only the author may remove their reaction (a foreign
// actor is 403), and a removed/absent row is a uniform 404
// (SPEC-0006 REQ "Authentication & Authorization").
func TestIntegrationUnreactByIDAuthorOnly(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "code", "alice", "x := 1")

	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "code_line", AnchorRef: json.RawMessage(`{"line":1}`), Emoji: "🎉"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("react = %d, want 201", resp.StatusCode)
	}
	rid := decodeReaction(t, resp).ID
	ridPath := srv.URL + "/v1/artifacts/" + id + "/reactions/" + strconv.FormatInt(rid, 10)

	// A different actor may not delete it.
	resp = do(t, http.MethodDelete, ridPath, "bob", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign delete = %d, want 403", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", env.Error.Code)
	}

	// The author deletes it.
	resp = do(t, http.MethodDelete, ridPath, "alice", nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("author delete = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Deleting again → uniform 404.
	resp = do(t, http.MethodDelete, ridPath, "alice", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("repeat delete = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestIntegrationCommentThreadReadBack posts a root comment and a one-level
// reply, then reads the thread back in order with the reply bound to its root
// (SPEC-0006 REQ "Threaded Comments").
func TestIntegrationCommentThreadReadBack(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "markdown", "alice", "# hello\n\nship it")

	sel := json.RawMessage(`{"start":10,"end":17,"quote":"ship it"}`)
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "alice",
		jsonReader(t, commentRequest{AnchorType: "text_selection", AnchorRef: sel, Body: "root comment"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("root comment = %d, want 201", resp.StatusCode)
	}
	var root commentResponse
	json.NewDecoder(resp.Body).Decode(&root)
	resp.Body.Close()

	// A reply omits the anchor to inherit its root's.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "bob",
		jsonReader(t, commentRequest{ParentID: &root.ID, Body: "agreed"}), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reply = %d, want 201", resp.StatusCode)
	}
	var reply commentResponse
	json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if reply.ParentID == nil || *reply.ParentID != root.ID {
		t.Fatalf("reply parent_id = %v, want %d", reply.ParentID, root.ID)
	}
	if reply.AnchorKey != root.AnchorKey || reply.AnchorType != root.AnchorType {
		t.Fatalf("reply anchor %s/%s did not inherit root %s/%s",
			reply.AnchorType, reply.AnchorKey, root.AnchorType, root.AnchorKey)
	}

	// Read the thread back: root first, then its reply.
	resp = do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/comments", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list comments = %d, want 200", resp.StatusCode)
	}
	list := decodeComments(t, resp)
	if len(list.Comments) != 2 {
		t.Fatalf("thread = %d comments, want 2", len(list.Comments))
	}
	if list.Comments[0].ID != root.ID || list.Comments[1].ID != reply.ID {
		t.Fatalf("thread order = [%d,%d], want [%d,%d]",
			list.Comments[0].ID, list.Comments[1].ID, root.ID, reply.ID)
	}
	if list.Comments[0].Body != "root comment" || list.Comments[1].Body != "agreed" {
		t.Fatalf("thread bodies = [%q,%q]", list.Comments[0].Body, list.Comments[1].Body)
	}
	if n := commentCount(t, srv.URL, id); n != 2 {
		t.Fatalf("artifact comment_count = %d, want 2", n)
	}
}

// TestIntegrationAnnotationCapabilityRejected proves the registry gates the
// anchor for both kinds: a comment on a reaction-only anchor and a reaction on a
// comment-only anchor are each validation_failed and persist nothing (SPEC-0006
// REQ "Registry-Gated Anchor Capabilities"). The webhook "reactable, never
// commentable" asymmetry is also exercised when a webhook artifact is available.
func TestIntegrationAnnotationCapabilityRejected(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "code", "alice", "func main() {}")

	// code_range is reaction-only on the code type → comment refused.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "alice",
		jsonReader(t, commentRequest{AnchorType: "code_range", AnchorRef: json.RawMessage(`{"start":1,"end":2}`), Body: "no"}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("comment on reaction-only anchor = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}

	// text_selection is comment-only on the code type → reaction refused.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "text_selection", AnchorRef: json.RawMessage(`{"start":1,"end":5,"quote":"func"}`), Emoji: "🔥"}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reaction on comment-only anchor = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}

	// Nothing persisted by the rejected writes.
	if n := reactionCount(t, srv.URL, id); n != 0 {
		t.Fatalf("reaction_count after rejected writes = %d, want 0", n)
	}
	if n := commentCount(t, srv.URL, id); n != 0 {
		t.Fatalf("comment_count after rejected writes = %d, want 0", n)
	}

	// Webhook asymmetry: a webhook artifact accepts a reaction on its request
	// anchor but refuses any comment.
	wh := createArtifact(t, srv.URL, "webhook", "alice", "POST /hook")
	whReq := json.RawMessage(`{"request_id":"req_7Kx9"}`)
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+wh+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "webhook_request", AnchorRef: whReq, Emoji: "👀"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("webhook_request reaction = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+wh+"/comments", "alice",
		jsonReader(t, commentRequest{AnchorType: "webhook_request", AnchorRef: whReq, Body: "nope"}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("webhook comment = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("webhook comment code = %q, want validation_failed", env.Error.Code)
	}
}

// TestIntegrationCommentOversizeRejected rejects an oversize comment body with
// 413 before buffering it (SPEC-0006 REQ "Request Body Size Limits").
func TestIntegrationCommentOversizeRejected(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())
	id := createArtifact(t, srv.URL, "code", "alice", "x")

	huge := strings.Repeat("a", maxAnnotationRequestBytes+1024)
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments", "alice",
		jsonReader(t, commentRequest{AnchorType: "artifact", Body: huge}), "application/json")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize comment = %d, want 413", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "payload_too_large" {
		t.Fatalf("code = %q, want payload_too_large", env.Error.Code)
	}
	if n := commentCount(t, srv.URL, id); n != 0 {
		t.Fatalf("comment_count after 413 = %d, want 0", n)
	}
}

// storeOpts is the common store configuration for the annotation integration
// tests: a generous upload cap so seeding artifacts never trips the 413 path
// under test here.
func storeOpts() store.Options { return store.Options{MaxUploadBytes: 1 << 20} }
