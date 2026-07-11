package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// spanRef builds a trajectory anchor_ref locator ({"span_id":"…"}).
func spanRef(spanID string) json.RawMessage {
	return json.RawMessage(`{"span_id":"` + spanID + `"}`)
}

// TestIntegrationTrajectoryAnchorsPersist wires the story's third task: reactions
// and comments anchored to trajectory spans flow through the K2–K4 annotation
// core, gated by the trajectory registry entry, and persist against stable
// span_ids. A tool-call span accepts a reaction (trajectory_toolcall); a
// reasoning span accepts a comment (trajectory_span); and the capability matrix
// rejects the inverse — a comment on a reaction-only turn and a reaction on a
// comment-only span (SPEC-0004 "Trajectory Annotation Anchors", SPEC-0006
// "Registry-Gated Anchor Capabilities").
func TestIntegrationTrajectoryAnchorsPersist(t *testing.T) {
	srv := testServer(t, noRateLimit(), storeOpts())

	// A closed batch run: s1 reasoning turn, s2 a bash tool call.
	run := decodeRun(t, do(t, http.MethodPost, srv.URL+"/v1/runs", "joe",
		jsonReader(t, runRequest{Mode: "batch", Prompt: "audit", StartedAt: fixedRunStart,
			Spans: []spanRequest{
				{SpanID: "s1", Category: "reason", Name: "plan", StartOffsetMS: 0, DurationMS: 100},
				{SpanID: "s2", Category: "exec", Tool: "bash", Name: "npm ls", StartOffsetMS: 100, DurationMS: 200},
			}}), "application/json"))

	// React 🔥 on the tool-call span → persists against a trajectory_toolcall anchor.
	resp := do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+run.ID+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "trajectory_toolcall", AnchorRef: spanRef("s2"), Emoji: "🔥"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("react on tool-call span = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Comment on the reasoning span → persists against a trajectory_span anchor.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+run.ID+"/comments", "bob",
		jsonReader(t, commentRequest{AnchorType: "trajectory_span", AnchorRef: spanRef("s1"), Body: "why plan first?"}),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("comment on reasoning span = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Both persist and read back anchored to their span_ids.
	tallies := decodeTallies(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+run.ID+"/reactions", "", nil, ""))
	if len(tallies.Reactions) != 1 || tallies.Reactions[0].AnchorType != "trajectory_toolcall" ||
		tallies.Reactions[0].Emoji != "🔥" || tallies.Reactions[0].Count != 1 {
		t.Fatalf("tallies = %+v, want one 🔥 on trajectory_toolcall", tallies.Reactions)
	}
	comments := decodeComments(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+run.ID+"/comments", "", nil, ""))
	if len(comments.Comments) != 1 || comments.Comments[0].AnchorType != "trajectory_span" ||
		comments.Comments[0].Body != "why plan first?" {
		t.Fatalf("comments = %+v, want one comment on trajectory_span", comments.Comments)
	}

	// The trajectory artifact's rollups reflect them, exposed separately.
	if rc := reactionCount(t, srv.URL, run.ID); rc != 1 {
		t.Fatalf("reaction_count = %d, want 1", rc)
	}
	if cc := commentCount(t, srv.URL, run.ID); cc != 1 {
		t.Fatalf("comment_count = %d, want 1", cc)
	}

	// Capability matrix, negative half: a comment on a reaction-only turn and a
	// reaction on a comment-only span are both rejected as validation failures,
	// persisting nothing.
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+run.ID+"/comments", "bob",
		jsonReader(t, commentRequest{AnchorType: "trajectory_turn", AnchorRef: spanRef("s1"), Body: "nope"}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("comment on reaction-only turn = %d, want 400", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", env.Error.Code)
	}
	resp = do(t, http.MethodPost, srv.URL+"/v1/artifacts/"+run.ID+"/reactions", "alice",
		jsonReader(t, reactionRequest{AnchorType: "trajectory_span", AnchorRef: spanRef("s1"), Emoji: "👀"}),
		"application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("react on comment-only span = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Nothing changed from the rejected inverse attempts.
	if rc := reactionCount(t, srv.URL, run.ID); rc != 1 {
		t.Fatalf("reaction_count after rejected inverse = %d, want 1", rc)
	}
	if cc := commentCount(t, srv.URL, run.ID); cc != 1 {
		t.Fatalf("comment_count after rejected inverse = %d, want 1", cc)
	}
}
