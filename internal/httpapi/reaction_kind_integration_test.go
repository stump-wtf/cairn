package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0022 (per-kind ownership), SPEC-0016 EV-6 "Agent cannot
// occupy the human's row", "Agent cannot withdraw the human's approval",
// "Agent cannot remove by id"; EV-4 "Client cannot assert the kind"; SPEC-0009
// REQ "Actor Captures Human and On-Behalf-Of Model".

// TestIntegrationReactionPerKindOwnership drives EV-6 through the real REST and
// MCP surfaces with one human, alice, holding both a browser session and
// bearer credentials. Her bearer calls resolve to the same actor_id as her
// session, so only the stored, server-derived kind keeps her agent's 👍 and
// her own 👍 apart.
func TestIntegrationReactionPerKindOwnership(t *testing.T) {
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), store.Options{MaxUploadBytes: 1 << 20})
	cfg := mcpConfig()
	cfg.DevInsecureBearerAuth = true // bearer "alice" is alice's agent (api_token)
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	id := createArtifact(t, srv.URL, "markdown", "alice", "# plan\n\nship it")
	reactions := srv.URL + "/v1/artifacts/" + id + "/reactions"

	// alice's browser: a dev-login session, CSRF double-submit on writes.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, browser, "alice", "devpass").Body.Close()
	sessionDo := func(method, url string, body io.Reader) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(csrfHeaderName, cookieValue(t, browser, srv.URL, csrfCookieName))
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatalf("session %s %s: %v", method, url, err)
		}
		return resp
	}
	tally := func(resp *http.Response) tallyView {
		t.Helper()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list reactions = %d, want 200", resp.StatusCode)
		}
		tr := decodeTallies(t, resp)
		if len(tr.Reactions) != 1 {
			t.Fatalf("tallies = %+v, want one 👍 tally", tr.Reactions)
		}
		return tr.Reactions[0]
	}

	// 1. alice's agent reacts 👍 first, with a hostile body claiming to be
	// human. The kind is derived, never asserted; on_behalf_of is kept.
	resp := do(t, http.MethodPost, reactions, "alice", strings.NewReader(
		`{"anchor_type":"artifact","emoji":"👍","actor_kind":"human","auth":"session","on_behalf_of":"claude-code/1.0"}`),
		"application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent react = %d, want 201", resp.StatusCode)
	}
	agentRow := decodeReaction(t, resp)
	if agentRow.ActorID != "alice" || agentRow.ActorKind != "agent" || agentRow.OnBehalfOf != "claude-code/1.0" {
		t.Fatalf("agent reaction = %+v, want alice/agent on behalf of claude-code/1.0", agentRow)
	}

	// 2. "Agent cannot occupy the human's row": alice's own click is a NEW
	// row (201), not the 200 no-op of a duplicate, and a web-session reaction
	// records an empty on_behalf_of.
	resp = sessionDo(http.MethodPost, reactions, strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("human react after agent = %d, want 201 (a new row)", resp.StatusCode)
	}
	humanRow := decodeReaction(t, resp)
	if humanRow.ActorID != "alice" || humanRow.ActorKind != "human" || humanRow.OnBehalfOf != "" || humanRow.ID == agentRow.ID {
		t.Fatalf("human reaction = %+v (agent row %d), want a separate alice/human row with no on_behalf_of", humanRow, agentRow.ID)
	}
	// A second click is still idempotent per kind.
	resp = sessionDo(http.MethodPost, reactions, strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat human react = %d, want 200", resp.StatusCode)
	}
	if again := decodeReaction(t, resp); again.ID != humanRow.ID {
		t.Fatalf("repeat human react returned row %d, want %d", again.ID, humanRow.ID)
	}
	if n := reactionCount(t, srv.URL, id); n != 2 {
		t.Fatalf("reaction_count = %d, want 2", n)
	}
	if got := tally(sessionDo(http.MethodGet, reactions, nil)); got.Count != 2 || got.HumanCount != 1 || got.AgentCount != 1 || !got.Reacted {
		t.Fatalf("browser tally = %+v, want count 2 (1 human, 1 agent), reacted", got)
	}

	// 3. "Agent cannot remove by id": 403, and nothing is deleted.
	humanPath := reactions + "/" + strconv.FormatInt(humanRow.ID, 10)
	resp = do(t, http.MethodDelete, humanPath, "alice", nil, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent delete of the human row = %d, want 403", resp.StatusCode)
	}
	if env := decodeError(t, resp); env.Error.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", env.Error.Code)
	}
	if n := reactionCount(t, srv.URL, id); n != 2 {
		t.Fatalf("reaction_count after refused delete = %d, want 2", n)
	}

	// 4. "Agent cannot withdraw the human's approval": the agent's un-react
	// by value removes only its own row.
	resp = do(t, http.MethodDelete, reactions, "alice",
		strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`), "application/json")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("agent un-react = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if got := tally(sessionDo(http.MethodGet, reactions, nil)); got.Count != 1 || got.HumanCount != 1 || got.AgentCount != 0 || !got.Reacted {
		t.Fatalf("tally after agent un-react = %+v, want alice's human row alone", got)
	}
	// The agent's own view of "did I react" follows its kind, not the id.
	if got := tally(do(t, http.MethodGet, reactions, "alice", nil, "")); got.Reacted {
		t.Fatalf("agent tally = %+v, want reacted=false: the remaining row is the human's", got)
	}

	// 5. MCP: an agent reaction records the human principal as actor and the
	// connected client as on_behalf_of.
	token := mintMCPToken(t, srv, "alice", []string{oauth.ScopeArtifactsRead, oauth.ScopeAnnotationsWrite})
	sess := mcpClient(t, srv, token, nil, "obo-probe")
	res := callTool(t, sess, "artifact_react", map[string]any{"id": id, "anchor_type": "artifact", "emoji": "👍"})
	if res.IsError {
		t.Fatalf("artifact_react: %s", toolText(t, res))
	}
	var mcpRow mcpReactOutput
	decodeToolJSON(t, res, &mcpRow)
	if mcpRow.ActorID != "alice" || mcpRow.ActorKind != "agent" || !strings.HasPrefix(mcpRow.OnBehalfOf, "obo-probe") {
		t.Fatalf("mcp reaction = %+v, want alice/agent on behalf of obo-probe", mcpRow)
	}
	if mcpRow.ID == humanRow.ID {
		t.Fatal("the MCP agent reaction landed on the human's row")
	}

	// 5b. The read path carries per-row provenance on request: an anonymous
	// link read of ?include=reactors shows alice's human row and her agent's
	// MCP row apart, with the agent's on_behalf_of and none for the browser.
	// Without the parameter the tally has no reactors; any other value is 400.
	if got := tally(do(t, http.MethodGet, reactions, "", nil, "")); got.Reactors != nil {
		t.Fatalf("default tally = %+v, want no reactors without ?include=reactors", got)
	}
	resp = do(t, http.MethodGet, reactions+"?include=everything", "", nil, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("?include=everything = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	detail := tally(do(t, http.MethodGet, reactions+"?include=reactors", "", nil, ""))
	if detail.Count != 2 || len(detail.Reactors) != 2 {
		t.Fatalf("detailed tally = %+v, want count 2 with two reactors", detail)
	}
	h, a := detail.Reactors[0], detail.Reactors[1]
	if h.ID != humanRow.ID || h.ActorID != "alice" || h.ActorKind != "human" || h.OnBehalfOf != "" {
		t.Fatalf("first reactor = %+v, want alice's human row %d with no on_behalf_of", h, humanRow.ID)
	}
	if a.ID != mcpRow.ID || a.ActorID != "alice" || a.ActorKind != "agent" || !strings.HasPrefix(a.OnBehalfOf, "obo-probe") {
		t.Fatalf("second reactor = %+v, want the MCP agent row %d on behalf of obo-probe", a, mcpRow.ID)
	}

	// 6. The human removes her own row by id; the agent's MCP row stands.
	resp = sessionDo(http.MethodDelete, humanPath, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("human delete of her own row = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	got := tally(do(t, http.MethodGet, reactions, "", nil, ""))
	if got.Count != 1 || got.AgentCount != 1 || got.HumanCount != 0 || got.Reacted {
		t.Fatalf("anonymous tally = %+v, want the agent row alone and reacted=false", got)
	}

	// The REST read of the comment shape carries the kind too.
	resp = sessionDo(http.MethodPost, srv.URL+"/v1/artifacts/"+id+"/comments",
		strings.NewReader(`{"anchor_type":"artifact","body":"approved","actor_kind":"agent"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("human comment = %d, want 201", resp.StatusCode)
	}
	var c commentResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	resp.Body.Close()
	if c.ActorKind != "human" {
		t.Fatalf("session comment actor_kind = %q, want human", c.ActorKind)
	}
	list := decodeComments(t, do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id+"/comments", "", nil, ""))
	if len(list.Comments) != 1 || list.Comments[0].ActorKind != "human" {
		t.Fatalf("listed comments = %+v, want one human comment", list.Comments)
	}
}
