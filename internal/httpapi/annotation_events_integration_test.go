package httpapi

import (
	"context"
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

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/pat"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0022 (annotation lifecycle events), SPEC-0016 EV-2 "MCP
// reaction emits", EV-4 "Human PAT is an agent", "Browser session is human",
// "Client cannot assert the kind", EV-5 "Agent thumbs-up is not an approval",
// "Human thumbs-up with skin tone is an approval", "Withdrawn approval is
// announced", EV-6 "Agent cannot occupy the human's row", "Agent cannot
// withdraw the human's approval", EV-7 "No env target receives the new kinds".

// ofKind returns, in emission order, the events of kind k about one public id.
func (s *lifecycleSpy) ofKind(id string, k event.Kind) []event.Event {
	var out []event.Event
	for _, ev := range s.forSubject(id) {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// last returns the most recent event of kind k about id, failing if none.
func (s *lifecycleSpy) last(t *testing.T, id string, k event.Kind) event.Event {
	t.Helper()
	evs := s.ofKind(id, k)
	if len(evs) == 0 {
		t.Fatalf("no %s event for %s", k, id)
	}
	return evs[len(evs)-1]
}

// hostileFields is what a bearer client that wants to be counted as a human
// would add to an annotation body: every field it could hope the server reads.
const hostileFields = `"actor_kind":"human","auth":"session","ambient":true,"is_agent":false,"on_behalf_of":"alice"`

// hostileDo sends a bearer request that also carries every client-controlled
// hint a forger would try: headers and query parameters claiming a session.
func hostileDo(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url+"?actor_kind=human&auth=session", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cairn-Actor-Kind", "human")
	req.Header.Set("X-Cairn-Auth", "session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// browserSession logs actor in with the dev password and returns a request
// function that sends the CSRF double-submit header on every call.
func browserSession(t *testing.T, srv *httptest.Server, actor string) func(method, url, body string) *http.Response {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	doLogin(t, srv, client, actor, "devpass").Body.Close()
	return func(method, url, body string) *http.Response {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, url, rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(csrfHeaderName, cookieValue(t, client, srv.URL, csrfCookieName))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("session %s %s: %v", method, url, err)
		}
		return resp
	}
}

// decodeComment decodes a created comment and closes the body.
func decodeComment(t *testing.T, resp *http.Response) commentResponse {
	t.Helper()
	defer resp.Body.Close()
	var c commentResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	return c
}

// wantStatus closes resp and fails unless it has status want.
func wantStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s = %d, want %d", what, resp.StatusCode, want)
	}
}

// assertApproval checks a reaction event's derived pair and approval bits.
func assertApproval(t *testing.T, ev event.Event, auth event.AuthMethod, kind event.ActorKind, class, approval bool) {
	t.Helper()
	if ev.Actor.Auth != auth || ev.Actor.Kind != kind {
		t.Errorf("%s (auth, kind) = (%q, %q), want (%q, %q)", ev.Kind, ev.Actor.Auth, ev.Actor.Kind, auth, kind)
	}
	if ev.Reaction == nil {
		t.Fatalf("%s carries no reaction payload", ev.Kind)
	}
	if ev.Reaction.ApprovalClass != class || ev.Reaction.Approval != approval {
		t.Errorf("%s (approval_class, approval) = (%v, %v), want (%v, %v)",
			ev.Kind, ev.Reaction.ApprovalClass, ev.Reaction.Approval, class, approval)
	}
}

// annotationEventServer is the full adapter with a lifecycle spy on
// Config.Events, a dev-login password, and alice's bearer credentials: a PAT
// minted is_agent=false, one minted is_agent=true, an MCP OAuth token, both
// CAIRN_API_TOKENS roles, and the dev bearer.
type annotationEventServer struct {
	srv                         *httptest.Server
	spy                         *lifecycleSpy
	humanPAT, agentPAT, oauthTk string
}

func newAnnotationEventServer(t *testing.T) annotationEventServer {
	t.Helper()
	spy := &lifecycleSpy{}
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), store.Options{MaxUploadBytes: 1 << 20})
	cfg := mcpConfig()
	cfg.Events = spy
	cfg.APITokens = []APIToken{
		{Secret: "static-human-secret", ActorID: "alice"},
		{Secret: "static-agent-secret", ActorID: "alice", IsAgent: true},
	}
	cfg.DevInsecureBearerAuth = true
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	patSvc := pat.NewService(pool)
	scopes := []string{oauth.ScopeArtifactsRead, oauth.ScopeArtifactsWrite, oauth.ScopeAnnotationsWrite}
	humanPAT, _, err := patSvc.Create(context.Background(), "alice", "cli", scopes, false)
	if err != nil {
		t.Fatalf("create human PAT: %v", err)
	}
	agentPAT, _, err := patSvc.Create(context.Background(), "alice", "agent", scopes, true)
	if err != nil {
		t.Fatalf("create agent PAT: %v", err)
	}
	return annotationEventServer{srv: srv, spy: spy, humanPAT: humanPAT, agentPAT: agentPAT,
		oauthTk: mintMCPToken(t, srv, "alice", scopes)}
}

// TestIntegrationAnnotationEventsActorKind is SPEC-0016 EV-4 on the annotation
// events, end to end through REST, MCP and the browser. #304 could observe the
// derived (auth, kind) pair only on artifact.created; here every bearer
// credential comments, reacts and un-reacts with a hostile body, hostile
// headers and hostile query parameters, and every event still carries the
// credential's own pair. Only the browser session is human, and only its
// approval-class reaction is an approval.
func TestIntegrationAnnotationEventsActorKind(t *testing.T) {
	es := newAnnotationEventServer(t)
	srv, spy := es.srv, es.spy
	id := createArtifact(t, srv.URL, "markdown", "alice", "# plan\n\nship it")
	base := srv.URL + "/v1/artifacts/" + id

	cases := []struct {
		name     string
		token    string
		wantAuth event.AuthMethod
	}{
		{"PAT minted is_agent=false", es.humanPAT, event.AuthPAT},
		{"PAT minted is_agent=true", es.agentPAT, event.AuthPAT},
		{"OAuth access token", es.oauthTk, event.AuthOAuth},
		{"CAIRN_API_TOKENS human role", "static-human-secret", event.AuthAPIToken},
		{"CAIRN_API_TOKENS agent role", "static-agent-secret", event.AuthAPIToken},
		{"dev insecure bearer", "alice", event.AuthAPIToken},
	}
	for i, tc := range cases {
		t.Run("REST "+tc.name, func(t *testing.T) {
			// "Client cannot assert the kind", through the real handler: the
			// stored comment and its comment.created are the credential's.
			body := "probe " + strconv.Itoa(i)
			resp := hostileDo(t, http.MethodPost, base+"/comments", tc.token,
				`{"anchor_type":"artifact","body":"`+body+`",`+hostileFields+`}`)
			if resp.StatusCode != http.StatusCreated {
				resp.Body.Close()
				t.Fatalf("hostile comment = %d, want 201", resp.StatusCode)
			}
			c := decodeComment(t, resp)
			if c.ActorKind != "agent" || c.OnBehalfOf != "alice" {
				t.Errorf("stored comment actor_kind %q on_behalf_of %q, want agent, alice (asserted, kept for display)",
					c.ActorKind, c.OnBehalfOf)
			}
			var created *event.Event
			for _, ev := range spy.ofKind(id, event.CommentCreated) {
				if ev.Comment != nil && ev.Comment.ID == c.ID {
					created = &ev
				}
			}
			if created == nil {
				t.Fatalf("no comment.created for comment %d", c.ID)
			}
			if created.Actor.Kind != event.KindAgent || created.Actor.Auth != tc.wantAuth ||
				created.Actor.ID != "alice" || created.Comment.Body != body {
				t.Errorf("comment.created actor %+v body %q, want alice as a %s agent, body %q",
					created.Actor, created.Comment.Body, tc.wantAuth, body)
			}

			// A hostile 👍 is in the class but never an approval, and neither
			// is its withdrawal.
			before := len(spy.ofKind(id, event.ReactionAdded))
			resp = hostileDo(t, http.MethodPost, base+"/reactions", tc.token,
				`{"anchor_type":"artifact","emoji":"👍🏽",`+hostileFields+`}`)
			wantStatus(t, resp, http.StatusCreated, "hostile react")
			added := spy.ofKind(id, event.ReactionAdded)
			if len(added) != before+1 {
				t.Fatalf("reaction.added events %d -> %d, want one more", before, len(added))
			}
			assertApproval(t, added[len(added)-1], tc.wantAuth, event.KindAgent, true, false)

			resp = hostileDo(t, http.MethodDelete, base+"/reactions", tc.token,
				`{"anchor_type":"artifact","emoji":"👍🏽",`+hostileFields+`}`)
			wantStatus(t, resp, http.StatusNoContent, "hostile unreact")
			removed := spy.ofKind(id, event.ReactionRemoved)
			if len(removed) == 0 {
				t.Fatal("no reaction.removed after the un-react")
			}
			assertApproval(t, removed[len(removed)-1], tc.wantAuth, event.KindAgent, true, false)
		})
	}

	// MCP: artifact_react and artifact_comment are agent writes whatever the
	// credential, and the connected client's name is the on_behalf_of. Both
	// credentials are alice's agent, so each reacts with its own emoji: a
	// second agent ✅ from alice would be the silent duplicate of the first.
	for _, tc := range []struct {
		name     string
		token    string
		emoji    string
		wantAuth event.AuthMethod
	}{
		{"MCP OAuth", es.oauthTk, "✅", event.AuthOAuth},
		{"MCP PAT", es.humanPAT, "👍", event.AuthPAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := mcpClient(t, srv, tc.token, nil, "kind-probe")
			addedBefore := len(spy.ofKind(id, event.ReactionAdded))
			res := callTool(t, sess, "artifact_react", map[string]any{"id": id, "anchor_type": "artifact", "emoji": tc.emoji})
			if res.IsError {
				t.Fatalf("artifact_react: %s", toolText(t, res))
			}
			added := spy.ofKind(id, event.ReactionAdded)
			if len(added) != addedBefore+1 {
				t.Fatalf("reaction.added events %d -> %d, want one more (EV-2 \"MCP reaction emits\")", addedBefore, len(added))
			}
			ev := added[len(added)-1]
			assertApproval(t, ev, tc.wantAuth, event.KindAgent, true, false)
			if ev.Actor.Channel != artifact.ChannelMCP || !strings.HasPrefix(ev.Actor.OnBehalfOf, "kind-probe") {
				t.Errorf("MCP reaction.added actor = %+v, want channel via MCP on behalf of the kind-probe client", ev.Actor)
			}

			res = callTool(t, sess, "artifact_comment", map[string]any{"id": id, "anchor_type": "artifact", "body": "from " + tc.name})
			if res.IsError {
				t.Fatalf("artifact_comment: %s", toolText(t, res))
			}
			var out mcpCommentOutput
			decodeToolJSON(t, res, &out)
			var got *event.Event
			for _, ev := range spy.ofKind(id, event.CommentCreated) {
				if ev.Comment != nil && ev.Comment.ID == out.ID {
					got = &ev
				}
			}
			if got == nil || got.Actor.Kind != event.KindAgent || got.Actor.Auth != tc.wantAuth {
				t.Fatalf("comment.created for MCP comment %d = %+v, want a %s agent", out.ID, got, tc.wantAuth)
			}
		})
	}

	// The browser session: the only human, and the only approval.
	t.Run("browser session", func(t *testing.T) {
		browser := browserSession(t, srv, "bob")

		resp := browser(http.MethodPost, base+"/reactions", `{"anchor_type":"artifact","emoji":"👍🏽"}`)
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("session react = %d, want 201", resp.StatusCode)
		}
		row := decodeReaction(t, resp)
		ev := spy.last(t, id, event.ReactionAdded)
		if ev.Reaction.ID != row.ID {
			t.Fatalf("last reaction.added is row %d, want the session's %d", ev.Reaction.ID, row.ID)
		}
		assertApproval(t, ev, event.AuthSession, event.KindHuman, true, true)

		// Withdrawn by value: announced as the approval it withdraws.
		wantStatus(t, browser(http.MethodDelete, base+"/reactions", `{"anchor_type":"artifact","emoji":"👍🏽"}`),
			http.StatusNoContent, "session unreact")
		ev = spy.last(t, id, event.ReactionRemoved)
		if ev.Reaction.ID != row.ID {
			t.Fatalf("last reaction.removed is row %d, want %d", ev.Reaction.ID, row.ID)
		}
		assertApproval(t, ev, event.AuthSession, event.KindHuman, true, true)

		// Withdrawn by id: the same.
		resp = browser(http.MethodPost, base+"/reactions", `{"anchor_type":"artifact","emoji":"✔️"}`)
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("session react ✔️ = %d, want 201", resp.StatusCode)
		}
		row = decodeReaction(t, resp)
		wantStatus(t, browser(http.MethodDelete, base+"/reactions/"+strconv.FormatInt(row.ID, 10), ""),
			http.StatusNoContent, "session unreact by id")
		ev = spy.last(t, id, event.ReactionRemoved)
		if ev.Reaction.ID != row.ID || ev.Reaction.Emoji != "✔️" {
			t.Fatalf("last reaction.removed = %+v, want row %d ✔️", *ev.Reaction, row.ID)
		}
		assertApproval(t, ev, event.AuthSession, event.KindHuman, true, true)

		resp = browser(http.MethodPost, base+"/comments", `{"anchor_type":"artifact","body":"lgtm"}`)
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("session comment = %d, want 201", resp.StatusCode)
		}
		c := decodeComment(t, resp)
		last := spy.last(t, id, event.CommentCreated)
		if last.Comment.ID != c.ID || last.Actor.Kind != event.KindHuman || last.Actor.Auth != event.AuthSession {
			t.Errorf("session comment.created = %+v by %+v, want comment %d by a session human", *last.Comment, last.Actor, c.ID)
		}
	})

	// Every annotation event names the artifact exactly as its creation would
	// (EV-3), and a read never emits.
	n := len(spy.forSubject(id))
	wantStatus(t, do(t, http.MethodGet, base+"/reactions", "", nil, ""), http.StatusOK, "list reactions")
	wantStatus(t, do(t, http.MethodGet, base+"/comments", "", nil, ""), http.StatusOK, "list comments")
	if got := len(spy.forSubject(id)); got != n {
		t.Errorf("reads emitted %d events", got-n)
	}
	for _, ev := range spy.forSubject(id) {
		if ev.Subject.ShareType != "markdown" || ev.Subject.WebPath != "/"+id || ev.Subject.OwnerID != "alice" {
			t.Errorf("%s subject = %+v, want the markdown artifact at /%s owned by alice", ev.Kind, ev.Subject, id)
		}
	}
}

// TestIntegrationAgentAndHumanApprovalRows is #311's end-to-end criterion: an
// MCP 👍 and then a browser 👍 from the same human are two rows and two
// events, approval false then true. The agent's un-react removes only its own
// row, announced as the non-approval it was, and the human's approval stands.
// The agent cannot delete the human's row by id either, and that refusal is
// silent.
func TestIntegrationAgentAndHumanApprovalRows(t *testing.T) {
	es := newAnnotationEventServer(t)
	srv, spy := es.srv, es.spy
	id := createArtifact(t, srv.URL, "markdown", "alice", "# plan")
	reactions := srv.URL + "/v1/artifacts/" + id + "/reactions"

	sess := mcpClient(t, srv, es.oauthTk, nil, "claude-code")
	res := callTool(t, sess, "artifact_react", map[string]any{"id": id, "anchor_type": "artifact", "emoji": "👍"})
	if res.IsError {
		t.Fatalf("artifact_react: %s", toolText(t, res))
	}
	var agentRow mcpReactOutput
	decodeToolJSON(t, res, &agentRow)

	browser := browserSession(t, srv, "alice")
	resp := browser(http.MethodPost, reactions, `{"anchor_type":"artifact","emoji":"👍"}`)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("human react after the agent = %d, want 201 (its own row)", resp.StatusCode)
	}
	humanRow := decodeReaction(t, resp)
	if humanRow.ID == agentRow.ID {
		t.Fatalf("human and agent share row %d", humanRow.ID)
	}

	added := spy.ofKind(id, event.ReactionAdded)
	if len(added) != 2 || added[0].Reaction.ID != agentRow.ID || added[1].Reaction.ID != humanRow.ID {
		t.Fatalf("reaction.added = %d events, want the agent's row %d then the human's %d", len(added), agentRow.ID, humanRow.ID)
	}
	assertApproval(t, added[0], event.AuthOAuth, event.KindAgent, true, false)
	assertApproval(t, added[1], event.AuthSession, event.KindHuman, true, true)

	// The agent cannot take the human's row by id, and nothing is announced.
	resp = do(t, http.MethodDelete, reactions+"/"+strconv.FormatInt(humanRow.ID, 10), es.oauthTk, nil, "")
	wantStatus(t, resp, http.StatusForbidden, "agent delete of the human's row")
	if n := len(spy.ofKind(id, event.ReactionRemoved)); n != 0 {
		t.Fatalf("a refused delete emitted %d reaction.removed", n)
	}

	// The agent's un-react (there is no MCP un-react tool, so the agent uses
	// its bearer on the REST toggle) removes its own row only.
	resp = do(t, http.MethodDelete, reactions, es.oauthTk,
		strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`), "application/json")
	wantStatus(t, resp, http.StatusNoContent, "agent unreact")
	removed := spy.ofKind(id, event.ReactionRemoved)
	if len(removed) != 1 || removed[0].Reaction.ID != agentRow.ID {
		t.Fatalf("reaction.removed = %d events, want exactly the agent's row %d", len(removed), agentRow.ID)
	}
	assertApproval(t, removed[0], event.AuthOAuth, event.KindAgent, true, false)

	resp = browser(http.MethodGet, reactions, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list reactions = %d", resp.StatusCode)
	}
	tr := decodeTallies(t, resp)
	if len(tr.Reactions) != 1 || tr.Reactions[0].HumanCount != 1 || tr.Reactions[0].AgentCount != 0 || !tr.Reactions[0].Reacted {
		t.Fatalf("tallies after the agent's un-react = %+v, want alice's human 👍 alone", tr.Reactions)
	}
}

// TestIntegrationAnnotationEventsReachTheEncoder wires the real outbound
// emitter the way cmd/cairnd does and proves the annotation events pass the
// ADR-0017 encoder: each is encoded and then held back from the env target
// (EV-7), which the per-kind drop counter records. A refused encode would
// increment nothing, and the env target receives only the creation.
func TestIntegrationAnnotationEventsReachTheEncoder(t *testing.T) {
	recv := newHookReceiver(t)
	em := outboundhook.New([]string{recv.srv.URL}, "", "https://cairn.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); em.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	cfg := mcpConfig()
	cfg.DevInsecureBearerAuth = true
	cfg.Events = em
	srv := testServer(t, cfg, store.Options{MaxUploadBytes: 1 << 20, Emitter: em})
	id := createArtifact(t, srv.URL, "markdown", "joe", "# wired")
	base := srv.URL + "/v1/artifacts/" + id

	wantStatus(t, do(t, http.MethodPost, base+"/comments", "joe",
		strings.NewReader(`{"anchor_type":"artifact","body":"`+strings.Repeat("é", 3000)+`"}`), "application/json"),
		http.StatusCreated, "comment")
	wantStatus(t, do(t, http.MethodPost, base+"/reactions", "joe",
		strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`), "application/json"),
		http.StatusCreated, "agent react")
	wantStatus(t, do(t, http.MethodDelete, base+"/reactions", "joe",
		strings.NewReader(`{"anchor_type":"artifact","emoji":"👍"}`), "application/json"),
		http.StatusNoContent, "agent unreact")
	browser := browserSession(t, srv, "bob")
	wantStatus(t, browser(http.MethodPost, base+"/reactions", `{"anchor_type":"artifact","emoji":"👍🏽"}`),
		http.StatusCreated, "human react")

	for k, want := range map[event.Kind]uint64{
		event.CommentCreated:  1,
		event.ReactionAdded:   2,
		event.ReactionRemoved: 1,
	} {
		if n := em.Dropped(k); n != want {
			t.Errorf("encoded-and-held %s = %d, want %d (fewer means the encoder refused one)", k, n, want)
		}
	}
	ev := recv.wait(t)
	if got := ev.hdr.Get("X-Cairn-Event"); got != string(event.ArtifactCreated) {
		t.Fatalf("env target received %q, want only artifact.created", got)
	}
	// Give a wrongly-enqueued annotation event time to arrive, then check the
	// target saw the creation alone.
	time.Sleep(200 * time.Millisecond)
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.seen) != 1 {
		t.Fatalf("env target received %d deliveries, want 1 (the creation)", len(recv.seen))
	}
}
