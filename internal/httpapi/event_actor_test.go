package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/oauth"
)

// Governing: ADR-0022 (server-derived actor kind), SPEC-0016 EV-4.

// TestPrincipalEventActor pins the one rule EV-4 rests on: the kind is human
// if and only if the principal is ambient (a CSRF-guarded session cookie).
// IsAgent does not enter into it, so a human-flagged bearer is still an agent.
func TestPrincipalEventActor(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want event.ActorKind
	}{
		{"session", Principal{ActorID: "alice", Channel: artifact.ChannelWeb, Ambient: true, Auth: event.AuthSession}, event.KindHuman},
		{"human PAT", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, IsAgent: false, Auth: event.AuthPAT}, event.KindAgent},
		{"agent PAT", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, IsAgent: true, Auth: event.AuthPAT}, event.KindAgent},
		{"oauth", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, IsAgent: true, Auth: event.AuthOAuth}, event.KindAgent},
		{"human api token", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, Auth: event.AuthAPIToken}, event.KindAgent},
		// A principal nobody classified is still never human.
		{"zero principal", Principal{ActorID: "alice"}, event.KindAgent},
		// Ambient alone is not enough: human needs the session stamp too.
		{"ambient, no auth", Principal{ActorID: "alice", Ambient: true}, event.KindAgent},
		{"ambient PAT", Principal{ActorID: "alice", Ambient: true, Auth: event.AuthPAT}, event.KindAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.EventActor()
			if got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.want)
			}
			if got.ID != tc.p.ActorID || got.Channel != tc.p.Channel || got.Auth != tc.p.Auth {
				t.Errorf("EventActor() = %+v, does not carry the principal's id/channel/auth", got)
			}
			if got.OnBehalfOf != "" {
				t.Errorf("OnBehalfOf = %q, want empty (left for the caller)", got.OnBehalfOf)
			}
			// A principal without an Auth stamp derives an actor every
			// producer refuses; a stamped one derives an acceptable actor.
			if ok := got.Check() == nil; ok != tc.p.Auth.Valid() {
				t.Errorf("Check(%+v) ok = %v, want %v", got, ok, tc.p.Auth.Valid())
			}
		})
	}
}

// TestStaticAuthenticatorsDeriveAuth covers the authenticators that need no
// database. The PAT, OAuth and session authenticators are covered end to end in
// TestIntegrationAuthenticatorActorKind.
func TestStaticAuthenticatorsDeriveAuth(t *testing.T) {
	tokens := NewTokenAuthenticator([]APIToken{
		{Secret: "human-secret", ActorID: "alice"},
		{Secret: "agent-secret", ActorID: "alice", IsAgent: true},
	})
	cases := []struct {
		name  string
		auth  Authenticator
		token string
	}{
		{"CAIRN_API_TOKENS human role", tokens, "human-secret"},
		{"CAIRN_API_TOKENS agent role", tokens, "agent-secret"},
		{"dev insecure bearer", DevActorAuthenticator{}, "alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			// Hostile hints a client might send: none of them may matter.
			r.Header.Set("X-Cairn-Actor-Kind", "human")
			r.Header.Set("X-Cairn-Auth", "session")
			p, err := tc.auth.Authenticate(r)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			a := p.EventActor()
			if a.Auth != event.AuthAPIToken || a.Kind != event.KindAgent {
				t.Errorf("(auth, kind) = (%q, %q), want (api_token, agent)", a.Auth, a.Kind)
			}
		})
	}
}

// TestMCPEventActorIsAlwaysAgent proves the MCP surface cannot yield a human
// actor, and records the credential the verifier accepted. The verifier's
// stash is the only input to Auth; nothing a tool call carries reaches it, and
// a stash the verifier never writes yields no Auth, which every producer
// refuses, rather than a guessed one.
func TestMCPEventActorIsAlwaysAgent(t *testing.T) {
	if a := mcpEventActor(nil); a.Kind != event.KindAgent || a.Auth != "" || a.Check() == nil {
		t.Errorf("nil request: actor %+v, want an unstamped agent that Check refuses", a)
	}
	withAuth := func(v any) *mcp.CallToolRequest {
		ti := &sdkauth.TokenInfo{UserID: "alice", Extra: map[string]any{}}
		if v != nil {
			ti.Extra[mcpExtraAuth] = v
		}
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}}
	}
	cases := []struct {
		name string
		req  *mcp.CallToolRequest
		want event.AuthMethod
	}{
		{"PAT verified", withAuth(string(event.AuthPAT)), event.AuthPAT},
		{"OAuth verified", withAuth(string(event.AuthOAuth)), event.AuthOAuth},
		// Anything the verifier did not stamp is left empty, not guessed: it
		// can never become a session, and Check refuses it.
		{"unstamped", withAuth(nil), ""},
		{"session claimed", withAuth(string(event.AuthSession)), ""},
		{"api token claimed", withAuth(string(event.AuthAPIToken)), ""},
		{"non-string", withAuth(42), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mcpEventActor(tc.req)
			if a.Kind != event.KindAgent || a.Auth != tc.want {
				t.Errorf("(auth, kind) = (%q, %q), want (%q, agent)", a.Auth, a.Kind, tc.want)
			}
			if ok := a.Check() == nil; ok != (tc.want != "") {
				t.Errorf("Check(%+v) ok = %v, want %v", a, ok, tc.want != "")
			}
			if a.ID != "alice" || a.Channel != artifact.ChannelMCP {
				t.Errorf("actor = %+v, want id alice on the mcp channel", a)
			}
		})
	}
}

// TestCommentActorIgnoresAssertedKind is SPEC-0016 EV-4 "Client cannot assert
// the kind" at the REST comment handler's seam: a hostile body is decoded
// exactly as handleComment decodes it, and the actor it produces carries the
// principal's derived kind and auth whatever the body claims. on_behalf_of is
// kept: it is the one asserted, display-only field.
func TestCommentActorIgnoresAssertedKind(t *testing.T) {
	const hostile = `{"anchor_type":"artifact","body":"lgtm",` +
		`"actor_kind":"human","auth":"session","ambient":true,"on_behalf_of":"claude-code/1.0"}`
	cases := []struct {
		name     string
		p        Principal
		wantKind event.ActorKind
	}{
		{"human PAT", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, Auth: event.AuthPAT}, event.KindAgent},
		{"OAuth", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, IsAgent: true, Auth: event.AuthOAuth}, event.KindAgent},
		{"API token", Principal{ActorID: "alice", Channel: artifact.ChannelAPI, Auth: event.AuthAPIToken}, event.KindAgent},
		{"session", Principal{ActorID: "alice", Channel: artifact.ChannelWeb, Ambient: true, Auth: event.AuthSession}, event.KindHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/artifacts/ABCDEFGH/comments", strings.NewReader(hostile))
			var req commentRequest
			if err := (&Server{}).decodeAnnotationBody(httptest.NewRecorder(), r, &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			a := commentActor(&tc.p, req)
			if a.Kind != tc.wantKind || a.Auth != tc.p.Auth {
				t.Errorf("(auth, kind) = (%q, %q), want (%q, %q)", a.Auth, a.Kind, tc.p.Auth, tc.wantKind)
			}
			if a.OnBehalfOf != "claude-code/1.0" {
				t.Errorf("OnBehalfOf = %q, want the asserted display value", a.OnBehalfOf)
			}
		})
	}
}

// TestMCPCreateRefusesUnstampedCredential proves the MCP create tools fail
// closed on a credential mcpTokenVerifier did not stamp: rather than create an
// artifact whose creation event carries no auth (or a guessed one), the call
// is refused as unauthorized before the store is touched. The server has no
// store, so the stamped positive controls get past the check and fail (or
// panic) further in, never as unauthorized.
//
// Governing: ADR-0022, SPEC-0016 EV-3, EV-4.
func TestMCPCreateRefusesUnstampedCredential(t *testing.T) {
	s := New(nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	req := func(stamp any) *mcp.CallToolRequest {
		ti := &sdkauth.TokenInfo{UserID: "alice", Scopes: []string{oauth.ScopeArtifactsWrite}, Extra: map[string]any{}}
		if stamp != nil {
			ti.Extra[mcpExtraAuth] = stamp
		}
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: ti}}
	}
	tools := map[string]func(*mcp.CallToolRequest) error{
		"artifact_create": func(r *mcp.CallToolRequest) error {
			_, _, err := s.mcpCreateArtifact(ctx, r, mcpCreateInput{Body: "hello"})
			return err
		},
		"bundle_create": func(r *mcp.CallToolRequest) error {
			_, _, err := s.mcpCreateBundle(ctx, r, mcpBundleCreateInput{})
			return err
		},
		"run_create": func(r *mcp.CallToolRequest) error {
			_, _, err := s.mcpCreateRun(ctx, r, mcpRunCreateInput{})
			return err
		},
	}
	isUnauthorized := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), messageFor(errs.CodeUnauthorized))
	}
	for tool, call := range tools {
		for _, stamp := range []any{nil, string(event.AuthSession), string(event.AuthAPIToken), "cookie", 42} {
			if err := call(req(stamp)); !isUnauthorized(err) {
				t.Errorf("%s with stamp %v = %v, want unauthorized", tool, stamp, err)
			}
		}
		for _, stamp := range []event.AuthMethod{event.AuthPAT, event.AuthOAuth} {
			panicked, err := callRecovering(func() error { return call(req(string(stamp))) })
			if !panicked && isUnauthorized(err) {
				t.Errorf("%s with stamp %q was refused as unauthorized, want it past the actor check", tool, stamp)
			}
		}
	}
}

// callRecovering runs fn, reporting a panic instead of propagating it.
func callRecovering(fn func() error) (panicked bool, err error) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	return false, fn()
}
