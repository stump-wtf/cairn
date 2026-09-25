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
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), store.Options{MaxUploadBytes: 1 << 20, Emitter: spy})
	cfg := mcpConfig()
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
