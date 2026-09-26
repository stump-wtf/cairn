package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/pat"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-4 "Human PAT is
// an agent", "Browser session is human", "Client cannot assert the kind".

// creationSpy records every creation event the store hands its emitter.
type creationSpy struct {
	mu  sync.Mutex
	evs []store.CreationEvent
}

func (c *creationSpy) EmitArtifactCreated(ev store.CreationEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
}

// byID returns the creation event for one public id.
func (c *creationSpy) byID(t *testing.T, id string) store.CreationEvent {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.evs {
		if ev.PublicID == id {
			return ev
		}
	}
	t.Fatalf("no creation event for %s", id)
	return store.CreationEvent{}
}

// TestIntegrationAuthenticatorActorKind maps every authenticator to the
// (auth, actor_kind) pair its creations carry, through the real /v1 and /mcp
// surfaces. Every bearer request also sends the hints a hostile client would
// try — a header, query parameters and an on_behalf_of — and none of them may
// move the derived pair.
func TestIntegrationAuthenticatorActorKind(t *testing.T) {
	spy := &creationSpy{}
	// Runs are minted and closed by the trajectory service, whose events reach
	// Config.Events rather than the store's creation emitter (SPEC-0016 EV-2).
	runSpy := &lifecycleSpy{}
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), store.Options{MaxUploadBytes: 1 << 20, Emitter: spy})
	cfg := mcpConfig()
	cfg.Events = runSpy
	cfg.APITokens = []APIToken{
		{Secret: "static-human-secret", ActorID: "alice"},
		{Secret: "static-agent-secret", ActorID: "alice", IsAgent: true},
	}
	cfg.DevInsecureBearerAuth = true
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(srv.Close)

	patSvc := pat.NewService(pool)
	writeScopes := []string{oauth.ScopeArtifactsRead, oauth.ScopeArtifactsWrite, oauth.ScopeAnnotationsWrite}
	humanPAT, _, err := patSvc.Create(context.Background(), "alice", "cli", writeScopes, false)
	if err != nil {
		t.Fatalf("create human PAT: %v", err)
	}
	agentPAT, _, err := patSvc.Create(context.Background(), "alice", "agent", writeScopes, true)
	if err != nil {
		t.Fatalf("create agent PAT: %v", err)
	}
	oauthToken := mintMCPToken(t, srv, "alice", writeScopes)

	// A hostile bearer create: every client-controlled hint says "human".
	bearerCreate := func(t *testing.T, token string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/v1/artifacts?type=text&actor_kind=human&auth=session&on_behalf_of=alice",
			strings.NewReader("hello"))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-Cairn-Actor-Kind", "human")
		req.Header.Set("X-Cairn-Auth", "session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("create status = %d, want 201", resp.StatusCode)
		}
		return decodeArtifact(t, resp).ID
	}

	// A hostile bearer run: opened and then closed with the same hints. The
	// run's artifact.created and its run.closed must both carry the derived
	// pair, whoever closes it.
	bearerRun := func(t *testing.T, token string) string {
		t.Helper()
		hostile := func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Cairn-Actor-Kind", "human")
			req.Header.Set("X-Cairn-Auth", "session")
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/runs?actor_kind=human&auth=session",
			jsonReader(t, runRequest{Mode: "open", Title: "probe", OnBehalfOf: "alice",
				Spans: []spanRequest{{SpanID: "s1", Category: "reason", DurationMS: 5}}}))
		req.Header.Set("Content-Type", "application/json")
		hostile(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open run: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("open run status = %d, want 201", resp.StatusCode)
		}
		id := decodeRun(t, resp).ID
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+id+"/close?actor_kind=human&auth=session", nil)
		hostile(req)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("close run: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("close run status = %d, want 200", resp.StatusCode)
		}
		return id
	}
	// assertRunPair checks both of a run's events carry (wantAuth, wantKind).
	assertRunPair := func(t *testing.T, id string, wantAuth event.AuthMethod, wantKind event.ActorKind) {
		t.Helper()
		for _, k := range []event.Kind{event.ArtifactCreated, event.RunClosed} {
			ev := runSpy.only(t, id, k)
			if ev.Actor.Auth != wantAuth || ev.Actor.Kind != wantKind {
				t.Errorf("%s (auth, kind) = (%q, %q), want (%q, %q)", k, ev.Actor.Auth, ev.Actor.Kind, wantAuth, wantKind)
			}
		}
	}

	cases := []struct {
		name     string
		token    string
		wantAuth event.AuthMethod
	}{
		{"PAT minted is_agent=false", humanPAT, event.AuthPAT},
		{"PAT minted is_agent=true", agentPAT, event.AuthPAT},
		{"OAuth access token", oauthToken, event.AuthOAuth},
		{"CAIRN_API_TOKENS human role", "static-human-secret", event.AuthAPIToken},
		{"CAIRN_API_TOKENS agent role", "static-agent-secret", event.AuthAPIToken},
		{"dev insecure bearer", "bob", event.AuthAPIToken},
	}
	for _, tc := range cases {
		t.Run("REST "+tc.name, func(t *testing.T) {
			ev := spy.byID(t, bearerCreate(t, tc.token))
			if ev.Auth != tc.wantAuth || ev.ActorKind != event.KindAgent {
				t.Errorf("(auth, kind) = (%q, %q), want (%q, agent)", ev.Auth, ev.ActorKind, tc.wantAuth)
			}
		})
		t.Run("REST run "+tc.name, func(t *testing.T) {
			assertRunPair(t, bearerRun(t, tc.token), tc.wantAuth, event.KindAgent)
		})
	}

	// MCP: the two credentials the transport accepts.
	for _, tc := range []struct {
		name     string
		token    string
		wantAuth event.AuthMethod
	}{
		{"MCP OAuth", oauthToken, event.AuthOAuth},
		{"MCP PAT", humanPAT, event.AuthPAT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := mcpClient(t, srv, tc.token, nil, "kind-probe")
			res := callTool(t, sess, "artifact_create", map[string]any{"body": "over mcp", "share_type": "text"})
			if res.IsError {
				t.Fatalf("artifact_create: %s", toolText(t, res))
			}
			var out mcpCreateOutput
			decodeToolJSON(t, res, &out)
			ev := spy.byID(t, out.ID)
			if ev.Auth != tc.wantAuth || ev.ActorKind != event.KindAgent {
				t.Errorf("(auth, kind) = (%q, %q), want (%q, agent)", ev.Auth, ev.ActorKind, tc.wantAuth)
			}
		})
		t.Run(tc.name+" run_create", func(t *testing.T) {
			sess := mcpClient(t, srv, tc.token, nil, "kind-probe")
			res := callTool(t, sess, "run_create", map[string]any{
				"title": "over mcp",
				"spans": []map[string]any{{"span_id": "s1", "category": "reason", "start_offset_ms": 0, "duration_ms": 5}},
			})
			if res.IsError {
				t.Fatalf("run_create: %s", toolText(t, res))
			}
			var out mcpRunOutput
			decodeToolJSON(t, res, &out)
			// A batch run is born closed: its creator is also its closer.
			assertRunPair(t, out.ID, tc.wantAuth, event.KindAgent)
		})
	}

	// The browser session: the only human.
	t.Run("browser session", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp := doLogin(t, srv, client, "alice", "devpass")
		resp.Body.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts?type=text", strings.NewReader("from the browser"))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set(csrfHeaderName, cookieValue(t, client, srv.URL, csrfCookieName))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("session create: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("session create status = %d, want 201", resp.StatusCode)
		}
		ev := spy.byID(t, decodeArtifact(t, resp).ID)
		if ev.Auth != event.AuthSession || ev.ActorKind != event.KindHuman {
			t.Errorf("(auth, kind) = (%q, %q), want (session, human)", ev.Auth, ev.ActorKind)
		}

		// A run opened and closed from the browser is the human's on both events.
		csrf := cookieValue(t, client, srv.URL, csrfCookieName)
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/runs",
			jsonReader(t, runRequest{Mode: "open", Title: "from the browser"}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeaderName, csrf)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("session open run: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("session open run status = %d, want 201", resp.StatusCode)
		}
		runID := decodeRun(t, resp).ID
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/runs/"+runID+"/close", nil)
		req.Header.Set(csrfHeaderName, csrf)
		resp, err = client.Do(req)
		if err != nil {
			t.Fatalf("session close run: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("session close run status = %d, want 200", resp.StatusCode)
		}
		assertRunPair(t, runID, event.AuthSession, event.KindHuman)
	})

	// A bearer sent alongside a live session cookie is still an agent: the
	// explicit credential wins, so an agent cannot borrow the human's cookie
	// jar to be counted as the human.
	t.Run("bearer beside a session cookie", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp := doLogin(t, srv, client, "alice", "devpass")
		resp.Body.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts?type=text", strings.NewReader("mixed"))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer "+humanPAT)
		req.Header.Set(csrfHeaderName, cookieValue(t, client, srv.URL, csrfCookieName))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("mixed create: %v", err)
		}
		if resp.StatusCode != http.StatusCreated {
			resp.Body.Close()
			t.Fatalf("mixed create status = %d, want 201", resp.StatusCode)
		}
		ev := spy.byID(t, decodeArtifact(t, resp).ID)
		if ev.Auth != event.AuthPAT || ev.ActorKind != event.KindAgent {
			t.Errorf("(auth, kind) = (%q, %q), want (pat, agent)", ev.Auth, ev.ActorKind)
		}
	})
}
