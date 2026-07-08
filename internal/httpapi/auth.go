package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// Principal is the authenticated caller. Channel is derived server-side from the
// authenticated surface — never from a client claim (SPEC-0002 "Channel is
// server-derived"). Scopes gate capabilities such as sharing:manage, which
// agents do not receive (SPEC-0002 "Agent cannot broaden sharing").
type Principal struct {
	ActorID string
	Channel artifact.Channel
	IsAgent bool
	Scopes  map[string]bool
}

// HasScope reports whether the principal holds scope.
func (p *Principal) HasScope(scope string) bool {
	return p.Scopes[scope]
}

// Authenticator resolves the authenticated principal for a request, or returns
// errs.ErrUnauthorized when credentials are absent or invalid. Real OAuth 2.1
// (bearer) and web sessions land in SPEC-0007/SPEC-0008 (#22/#27); this seam is
// what those adapters implement.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// BearerAuthenticator is a minimal development Authenticator: it treats an
// `Authorization: Bearer <token>` token as the actor id and derives the channel
// server-side as `via API` for this REST surface, ignoring any client-asserted
// channel. It is intentionally a stub — real token verification, identity
// resolution, and scope assignment arrive with OAuth (#22).
type BearerAuthenticator struct{}

// Authenticate implements Authenticator.
func (BearerAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return nil, errs.ErrUnauthorized
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if token == "" {
		return nil, errs.ErrUnauthorized
	}
	// Dev mapping: the bearer token IS the actor id. The channel is fixed to the
	// REST surface's `via API`, so a client cannot spoof provenance.
	return &Principal{
		ActorID: token,
		Channel: artifact.ChannelAPI,
		Scopes:  map[string]bool{"artifacts:write": true, "sharing:manage": true},
	}, nil
}

type principalCtxKey struct{}

// requireAuth is middleware that authenticates the request and stashes the
// principal in the context, or writes a 401 envelope. It guards mutating and
// workspace-scoped endpoints; link-capability reads are public.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			s.writeError(w, r, errs.ErrUnauthorized, nil)
			return
		}
		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFrom returns the authenticated principal stashed by requireAuth.
func principalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}
