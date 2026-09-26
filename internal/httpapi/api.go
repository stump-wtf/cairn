package httpapi

import (
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/httpapi/authprovider"
	"github.com/stump-wtf/cairn/internal/mcpsession"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/pat"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/session"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/trajectory"
	"github.com/stump-wtf/cairn/internal/webhook"
)

// Config tunes the REST adapter.
type Config struct {
	// BaseURL is the public origin used to build short URLs, e.g.
	// https://cairn.stump.wtf. No trailing slash.
	BaseURL string
	// MaxUploadBytes caps a single request body / upload part (413 above it).
	MaxUploadBytes int64
	// DefaultTTL is the artifact expiry assigned at create (ADR-0007).
	DefaultTTL time.Duration
	// MaxRequestedTTL bounds an explicit client-requested expiry (the CLI's
	// `--ttl` flag, SPEC-0008) sent as X-Cairn-Ttl-Seconds on POST
	// /v1/artifacts: the server remains authoritative over expiry (ADR-0007
	// "owner-adjustable ... subject to any workspace cap") — a request
	// outside (0, MaxRequestedTTL] is rejected as validation_failed rather
	// than silently clamped, so a caller never believes it got a longer TTL
	// than it did. This header is honored only on the REST create path (the
	// CLI/web surface); the MCP agent surface's artifact_create/
	// bundle_create schemas carry no TTL field at all and always get
	// DefaultTTL (internal/httpapi/mcp.go), matching the existing
	// human-vs-agent capability split (ADR-0004: e.g. delete is human-only).
	// Defaults to 30 days.
	MaxRequestedTTL time.Duration
	// RatePerSecond / RateBurst configure per-IP rate limiting; <= 0 disables.
	RatePerSecond float64
	RateBurst     int
	// MaxRunRequestBytes caps a trajectory run/append request body before it is
	// buffered, so an oversize batch is 413 rather than read into memory
	// (SPEC-0004 endpoint security). Defaults to 64 MiB.
	MaxRunRequestBytes int64
	// StreamHeartbeat is the interval between SSE heartbeat comments on the live
	// span stream, which keep proxies from idling the connection out and let the
	// server notice a vanished client on the next write. Defaults to 15s.
	StreamHeartbeat time.Duration
	// DevLoginPassword is the shared secret the MVP dev login (SPEC-0001,
	// ADR-0004) accepts for any actor id. Demoted to a local-dev-only fallback
	// by ADR-0013: it is honored only when OIDC is unconfigured (see
	// loginEnabled). An empty value disables interactive web login entirely.
	DevLoginPassword string
	// SessionTTL is the lifetime of a web session and its cookies. Defaults to 7
	// days.
	SessionTTL time.Duration
	// OIDC relying-party config (ADR-0013): Cairn authenticates humans directly
	// against Pocket ID rather than an external forward-auth proxy. OIDCIssuer
	// gates the whole feature — empty means OIDC is not configured and
	// EnableOIDC is a no-op, leaving the dev-password fallback as the only login
	// path. The redirect URI is always BaseURL + /auth/callback (derived, never
	// separately configured).
	OIDCIssuer       string
	OIDCClientID     string // defaults to "cairn" when empty
	OIDCClientSecret string
	// GitHub human login (SPEC-0012): a second provider beside Pocket ID.
	// GitHubConfigured() gates EnableGitHub — the provider joins the registry
	// (and the login page renders its button) only when both are set. The
	// redirect URI is derived as BaseURL + /auth/callback like OIDC's.
	GitHubClientID     string
	GitHubClientSecret string
	// APITokens are the static bearer credentials the API/MCP surface accepts
	// (ADR-0004 MVP token seam). Each secret maps to an actor and role; an absent
	// set means the bearer surface rejects every token (fail closed). A raw
	// bearer string is never trusted as an actor id.
	APITokens []APIToken
	// DevInsecureBearerAuth, when true, additionally trusts a raw bearer token AS
	// the actor id (DevActorAuthenticator) after the verifying TokenAuthenticator
	// declines. It is a development/test-only shortcut that MUST stay false in
	// production; the production default verifies every bearer token.
	DevInsecureBearerAuth bool
	// OAuth 2.1 authorization-server tuning (SPEC-0007, ADR-0004).
	// AccessTokenTTL is the short audience-bound access-token lifetime (default
	// ~1h); RefreshTokenTTL the rotating refresh-token lifetime (default 30d).
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// OAuthRatePerSecond / OAuthRateBurst configure the dedicated per-IP rate
	// limiter on the OAuth bootstrap endpoints (register/token/revoke/authorize),
	// throttled tighter than the general surface to blunt client-spraying and
	// code/refresh brute force (SPEC-0007 REQ "Rate Limiting"). Defaults: 10/s,
	// burst 30; always on when the authorization server is wired.
	OAuthRatePerSecond float64
	OAuthRateBurst     int
	// HookIngressRatePerSecond / HookIngressRateBurst configure the open
	// webhook ingress's dedicated PER-SOURCE-IP limiter, and
	// HookEndpointRatePerSecond / HookEndpointRateBurst its PER-ENDPOINT
	// limiter (SPEC-0005 REQ "Rate Limiting": "rate-limited per-endpoint and
	// per-source-IP"). Both are separate from the general per-IP limiter
	// (RatePerSecond/RateBurst) because the anonymous-write ingress is the
	// single most exposed surface in Cairn and must carry its own budget,
	// never share one with authenticated traffic. Defaults: 5/s burst 20
	// per-IP, 10/s burst 50 per-endpoint; always on (a deployment that truly
	// wants no ingress throttling must set both to a very high value —
	// zero/negative still falls back to the default rather than disabling
	// it, matching the OAuth limiter's always-on posture on this
	// internet-facing surface).
	HookIngressRatePerSecond  float64
	HookIngressRateBurst      int
	HookEndpointRatePerSecond float64
	HookEndpointRateBurst     int
	// Redaction is the ingest secret scanner the comment and trace write paths
	// mask credentials with (ADR-0023, SPEC-0017), and Metrics counts its
	// scans. cairnd always sets Redaction; without it those paths store what
	// they are given and record the outcome "unscanned".
	Redaction *redact.Scanner
	Metrics   *metrics.Registry
}

// Server is the /v1 REST adapter over the core store and the ADR-0011 web app
// shell. Both are thin projections of the same core (ADR-0003): the JSON API and
// the HTML shell share the store, registry, and annotation service, so they can
// never disagree about an artifact's facts.
type Server struct {
	store   *store.Store
	annot   *annotation.Service
	traj    *trajectory.Service
	hook    *webhook.Service
	reg     *sharetype.Registry
	auth    Authenticator
	cfg     Config
	limiter *rateLimiter
	log     *slog.Logger
	now     func() time.Time
	webTmpl *template.Template
	// Web session surface (SPEC-0001, ADR-0004). sessions is the server-side
	// store, verifier the swap-in credential seam OAuth replaces, and
	// secureCookies gates the cookie Secure flag on the public origin's scheme.
	sessions      session.Store
	verifier      CredentialVerifier
	secureCookies bool
	// oidc is the OIDC relying-party wiring (ADR-0013): nil until EnableOIDC
	// discovers the configured issuer (or forever nil when OIDC is not
	// configured), gating both the /auth/login|callback routes and whether the
	// dev-password fallback stays honored (loginEnabled).
	oidc *oidcRP
	// gh is the GitHub login provider (SPEC-0012): nil until EnableGitHub
	// wires it from config (or forever nil when GitHub is not configured),
	// gating ?provider=github at both /auth routes.
	gh *authprovider.GitHubProvider
	// OAuth 2.1 authorization server (SPEC-0007, ADR-0004): the in-process core
	// service the /oauth endpoints adapt, plus its dedicated tighter rate
	// limiter. Nil on storeless unit wirings (the AS persists in Postgres).
	oauth        *oauth.Service
	oauthLimiter *rateLimiter
	// hookIPLimiter / hookEndpointLimiter are the open webhook ingress's own
	// dedicated per-source-IP and per-endpoint limiters (SPEC-0005 REQ "Rate
	// Limiting"), separate from both the general per-IP limiter and the
	// OAuth one — the anonymous-write ingress is its own attack surface with
	// its own budget. Nil (disabled) only on storeless unit wirings where
	// s.hook is also nil; otherwise always constructed (see New).
	hookIPLimiter       *rateLimiter
	hookEndpointLimiter *rateLimiter
	// hookMaxBodyBytes is the open ingress's hard pre-buffering body-size cap
	// (SPEC-0005 REQ "Request Body Size Limits"): read from s.hook.MaxBodyBytes()
	// at construction so the HTTP-layer 413 cap can never drift from the cap
	// Capture itself enforces. Zero (storeless wirings, s.hook nil) disables the
	// ingress route entirely — see Handler.
	hookMaxBodyBytes int64
	// pat is the personal-access-token core (issue #74, ADR-0004 token seam):
	// the in-process service backing POST/GET/DELETE /v1/tokens and the
	// PATAuthenticator bearer surface. Nil on storeless unit wirings, like
	// oauth above.
	pat *pat.Service
	// mcpSessions is the MCP agent-session core (issue #76, SPEC-0007): the
	// in-process service backing session recording at MCP `initialize`,
	// per-tool-call activity counters, GET /v1/mcp/sessions, and the
	// Settings "Agent sessions" section. Nil on storeless unit wirings, like
	// oauth/pat above.
	mcpSessions *mcpsession.Service
	// mcpSrv is the MCP tool/resource server (SPEC-0007), built once by
	// mountMCP and shared by every /mcp request so server->client
	// notifications (e.g. resources/updated) can reach every connected
	// session. Nil until mountMCP runs; nil entirely when MCP is disabled
	// (mcpEnabled() false).
	mcpSrv *mcp.Server
}

// New constructs a Server. If auth is nil, a session-aware Authenticator is used
// when a store is present (bearer tokens for API/MCP/CLI, session cookies for the
// web, per SPEC-0001/ADR-0004), falling back to the bare BearerAuthenticator for
// storeless unit wirings; if logger is nil slog.Default() is used.
func New(st *store.Store, reg *sharetype.Registry, auth Authenticator, cfg Config, logger *slog.Logger) *Server {
	if reg == nil {
		reg = sharetype.Default()
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.BaseURL == "" {
		// This fallback is baked into every artifact URL the server hands out,
		// so it must be a host we actually control. It was https://cairn.sh,
		// which nobody here owns — meaning an unconfigured deployment minted
		// links pointing at a domain a stranger could register. A deployment
		// that is not the hosted service sets CAIRN_BASE_URL.
		cfg.BaseURL = "https://cairn.stump.wtf"
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 64 << 20
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 7 * 24 * time.Hour
	}
	if cfg.MaxRequestedTTL <= 0 {
		cfg.MaxRequestedTTL = 30 * 24 * time.Hour
	}
	if cfg.MaxRunRequestBytes <= 0 {
		cfg.MaxRunRequestBytes = 64 << 20
	}
	if cfg.StreamHeartbeat <= 0 {
		cfg.StreamHeartbeat = 15 * time.Second
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.OAuthRatePerSecond <= 0 {
		cfg.OAuthRatePerSecond = 10
	}
	if cfg.OAuthRateBurst <= 0 {
		cfg.OAuthRateBurst = 30
	}
	if cfg.HookIngressRatePerSecond <= 0 {
		cfg.HookIngressRatePerSecond = 5
	}
	if cfg.HookIngressRateBurst <= 0 {
		cfg.HookIngressRateBurst = 20
	}
	if cfg.HookEndpointRatePerSecond <= 0 {
		cfg.HookEndpointRatePerSecond = 10
	}
	if cfg.HookEndpointRateBurst <= 0 {
		cfg.HookEndpointRateBurst = 50
	}
	// The annotation, trajectory, and webhook cores are peers of the artifact
	// store, projected by this same adapter (SPEC-0006 REQ "Cross-Surface
	// Parity", SPEC-0004, SPEC-0005). Each shares the store's pool and registry
	// so its writes land in the same database and transaction domain and gate
	// anchors against the same capability matrix (ADR-0012 one binary, one
	// core); the trajectory and webhook services additionally spill their
	// oversized span outputs / captured bodies to the store's object store. A
	// nil store (unit tests that exercise only URL/auth helpers) leaves all
	// three nil.
	var (
		annot      *annotation.Service
		traj       *trajectory.Service
		hookSvc    *webhook.Service
		sessions   session.Store
		oauthSvc   *oauth.Service
		patSvc     *pat.Service
		mcpSessSvc *mcpsession.Service
	)
	if st != nil {
		annot = annotation.NewService(st.Pool(), reg, annotation.WithRedaction(cfg.Redaction, cfg.Metrics))
		traj = trajectory.NewService(st.Pool(), st.ObjectStore(), trajectory.Options{
			Registry:  reg,
			Redaction: cfg.Redaction,
			Metrics:   cfg.Metrics,
		})
		hookSvc = webhook.NewService(st.Pool(), st.ObjectStore(), webhook.Options{})
		// The web session store lives in the same Postgres as the core, so the
		// single binary carries its schema and a scaled deployment shares one
		// session table (ADR-0012).
		sessions = session.NewPostgresStore(st.Pool())
		// The OAuth 2.1 authorization server is an in-process peer of the core —
		// not a separate service (SPEC-0007 design "In-process adapter over the
		// core"). Access tokens are audience-bound to this deployment's public
		// origin (RFC 8707).
		oauthSvc = oauth.NewService(st.Pool(), cfg.BaseURL, oauth.Options{
			AccessTTL:  cfg.AccessTokenTTL,
			RefreshTTL: cfg.RefreshTokenTTL,
		})
		// Personal access tokens (issue #74) are a peer in-process core too: a
		// human-minted alternative to CAIRN_API_TOKENS, persisted in the same
		// Postgres pool.
		patSvc = pat.NewService(st.Pool())
		// MCP agent sessions (issue #76) are a peer in-process core too:
		// recorded by the MCP transport, read by the Settings page and
		// GET /v1/mcp/sessions, persisted in the same Postgres pool.
		mcpSessSvc = mcpsession.NewService(st.Pool())
	}
	// Auth seam (ADR-0004): a caller-supplied Authenticator wins; otherwise build
	// the bearer surface from configured static tokens (the verifying
	// TokenAuthenticator — a raw bearer is never trusted as an actor), then the
	// OAuth access tokens the authorization server issues, optionally chained
	// with the INSECURE dev shortcut when explicitly enabled. The bearer surface
	// backs both the standalone API wiring and the session-aware adapter, so the
	// web binary authenticates browser sessions AND verified bearer tokens.
	if auth == nil {
		bearers := chainAuthenticator{NewTokenAuthenticator(cfg.APITokens)}
		if patSvc != nil {
			bearers = append(bearers, &PATAuthenticator{svc: patSvc})
		}
		if oauthSvc != nil {
			bearers = append(bearers, &OAuthAuthenticator{svc: oauthSvc})
		}
		if cfg.DevInsecureBearerAuth {
			bearers = append(bearers, DevActorAuthenticator{})
		}
		var bearer Authenticator = bearers
		if sessions != nil {
			auth = &SessionAuthenticator{sessions: sessions, bearer: bearer}
		} else {
			auth = bearer
		}
	}
	// The open ingress's HTTP-layer body cap is read straight off the webhook
	// service's own configured cap (webhook.Service.MaxBodyBytes), never a
	// separately maintained number, so the 413-before-buffering guard can
	// never silently drift from what Capture itself accepts (SPEC-0005 REQ
	// "Request Body Size Limits"). Zero (hookSvc nil) leaves the ingress
	// route unmounted — see Handler.
	var hookMaxBodyBytes int64
	if hookSvc != nil {
		hookMaxBodyBytes = hookSvc.MaxBodyBytes()
	}
	return &Server{
		store:               st,
		annot:               annot,
		traj:                traj,
		hook:                hookSvc,
		reg:                 reg,
		auth:                auth,
		cfg:                 cfg,
		limiter:             newRateLimiter(cfg.RatePerSecond, cfg.RateBurst),
		log:                 logger,
		now:                 time.Now,
		webTmpl:             parseWebTemplates(),
		sessions:            sessions,
		verifier:            DevPasswordVerifier{Password: cfg.DevLoginPassword},
		secureCookies:       strings.HasPrefix(cfg.BaseURL, "https://"),
		oauth:               oauthSvc,
		oauthLimiter:        newRateLimiter(cfg.OAuthRatePerSecond, cfg.OAuthRateBurst),
		hookIPLimiter:       newRateLimiter(cfg.HookIngressRatePerSecond, cfg.HookIngressRateBurst),
		hookEndpointLimiter: newRateLimiter(cfg.HookEndpointRatePerSecond, cfg.HookEndpointRateBurst),
		hookMaxBodyBytes:    hookMaxBodyBytes,
		pat:                 patSvc,
		mcpSessions:         mcpSessSvc,
	}
}

// Handler returns the routed http.Handler for both surfaces this adapter
// serves: the /v1 REST/JSON API and the ADR-0011 web app shell. The two are
// separate route groups so each carries its OWN Content-Security-Policy — the
// API keeps the strict `default-src 'none'` (securityHeaders) while the HTML
// shell gets webCSP (self + the script/style/font origins HTMX+Alpine need),
// with neither policy loosening the other (SPEC-0001 REQ "Security Headers").
// The RequestID/RealIP/Recoverer and per-IP rate limiter are shared, so id
// resolution is throttled on both surfaces (SPEC-0001 REQ "Rate Limiting").
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(s.rateLimit)

	// The HTML web app shell (ADR-0011): its own CSP, the link-capability read
	// routes GET /{id} and GET /run/{id}, the landing, and the embedded assets.
	r.Group(func(r chi.Router) {
		r.Use(s.webSecurityHeaders)
		s.mountWeb(r)
	})

	// The /v1 REST/JSON API, under the strict API CSP. The OAuth 2.1 JSON
	// endpoints (discovery, DCR, token, revoke; SPEC-0007) share this strict
	// policy — only the browser-facing /oauth/authorize consent screen renders
	// HTML, and it lives in the web group above (see mountOAuthWeb).
	r.Group(func(r chi.Router) {
		r.Use(s.securityHeaders)
		s.mountAPI(r)
		s.mountOAuthJSON(r)
		// The MCP transport (SPEC-0007): tools + resource reads, OAuth-bearer
		// authenticated, under the same strict CSP as the rest of /v1.
		s.mountMCP(r)
	})

	// The webhook open ingress (SPEC-0005 HTTP endpoints table: `ANY /h/{id}`):
	// the single Public, anonymous-write route in Cairn, deliberately mounted
	// as its own top-level group — NOT under /v1, NOT under the web session
	// group — so it carries the strict /v1-style CSP (never the HTMX/Alpine
	// webCSP) but is exempt from requireAuth/enforceCSRF/session machinery
	// entirely (it's a machine ingress like /v1 and /mcp, bypassing Pocket
	// ID/OIDC by design; ADR-0010 "the endpoint is anonymous-write and
	// internet-facing"). Nil s.hook (storeless unit wirings) leaves it
	// unmounted, matching mountAPI's own s.hook guard.
	if s.hook != nil {
		r.Group(func(r chi.Router) {
			r.Use(s.securityHeaders)
			s.mountHookIngress(r)
		})
	}
	return r
}

// mountAPI registers the /v1 REST/JSON routes on the given router (already under
// the strict-CSP group).
func (s *Server) mountAPI(r chi.Router) {
	r.Route("/v1", func(r chi.Router) {
		// Mutating / workspace-scoped endpoints require authentication, the
		// matching capability scope (401 when unauthenticated, 403 when
		// authenticated but under-scoped), and — for state-changing writes — the
		// CSRF seam. enforceCSRF is a no-op for token (non-ambient) callers and the
		// guard the cookie-session surface plugs into, so a browser session cannot
		// be CSRF-tricked into creating or deleting an artifact (SPEC-0006 REQ
		// "CSRF Protection"; closes the gap left when only annotation writes were
		// guarded).
		r.With(s.requireAuth, s.requireScope(scopeArtifactsWrite), s.enforceCSRF).Post("/artifacts", s.handleCreate)
		// Delete is a human-only capability: the three-scope model (ADR-0004) grants
		// agents artifacts:write but NO delete scope, so requireHuman refuses every
		// agent token (403) — an agent can never delete on the human's behalf, while
		// human tokens and web sessions (both non-agent) retain it. requireScope keeps
		// the write-capability gate; requireHuman adds the human-only gate on top.
		r.With(s.requireAuth, s.requireScope(scopeArtifactsWrite), s.requireHuman, s.enforceCSRF).Delete("/artifacts/{id}", s.handleDelete)
		// The owner policy surface (issue #94, SPEC-0009 Endpoint Table): change
		// sharing, change TTL, and rotate the id. Gated on sharing:manage — the
		// human-only capability agentScopes never grants (ADR-0004 / SPEC-0007
		// "sharing:manage is human-only") — so every agent/PAT token is refused
		// with 403 before the store is even touched; the store's own
		// resolveOwned then enforces per-artifact ownership with a distinct 403
		// for an authenticated non-owner (SPEC-0009 Security Requirements).
		r.With(s.requireAuth, s.requireScope(scopeSharingManage), s.enforceCSRF).Patch("/artifacts/{id}/policy", s.handleUpdateVisibility)
		r.With(s.requireAuth, s.requireScope(scopeSharingManage), s.enforceCSRF).Patch("/artifacts/{id}/ttl", s.handleUpdateTTL)
		r.With(s.requireAuth, s.requireScope(scopeSharingManage), s.enforceCSRF).Post("/artifacts/{id}/rotate", s.handleRotateID)
		r.With(s.requireAuth).Get("/bin", s.handleBin)
		// Bearer-token identity round trip (cairn#21, SPEC-0008 "cairn whoami"):
		// any authenticated principal — static token, PAT, OAuth, or session —
		// resolves its own actor id and server-derived channel here, with no
		// workspace-scoping concerns since a principal can only ever be itself.
		r.With(s.requireAuth).Get("/whoami", s.handleAPIWhoami)

		// Personal access tokens (issue #74, ADR-0004 token seam): the human
		// Settings surface for minting/listing/revoking PATs. Management is
		// session/OIDC-authenticated only (requireHumanSession refuses every
		// bearer caller, including a PAT itself) and CSRF-guarded on writes,
		// per issue #74 ("session/OIDC-authenticated, CSRF-guarded"). Once
		// minted, the token authenticates on /v1 via PATAuthenticator in the
		// bearer chain (api.go New), not through this route group.
		r.With(s.requireHumanSession).Get("/tokens", s.handleListTokens)
		r.With(s.requireHumanSession, s.enforceCSRF).Post("/tokens", s.handleCreateToken)
		r.With(s.requireHumanSession, s.enforceCSRF).Delete("/tokens/{id}", s.handleRevokeToken)

		// MCP agent sessions (issue #76, ADR-0004 token seam): the human
		// Settings surface for seeing which agents are connected and ending
		// one (revokes its OAuth grant). Session/OIDC-authenticated only,
		// same reasoning as the tokens routes above.
		r.With(s.requireHumanSession).Get("/mcp/sessions", s.handleListMCPSessions)
		r.With(s.requireHumanSession, s.enforceCSRF).Delete("/mcp/sessions/{id}", s.handleEndMCPSession)

		// Link-capability reads: a valid id grants read; unknown/expired ids
		// return a uniform 404 (ADR-0007).
		r.Get("/artifacts/{id}", s.handleGet)
		r.Get("/artifacts/{id}/body", s.handleGetBody)
		r.Get("/artifacts/{id}/members/*", s.handleGetMember)

		// Annotations (SPEC-0006, ADR-0006). Writes are authenticated and pass
		// the CSRF seam (a no-op for token auth, the guard the future
		// cookie-session surface plugs into, #11); reads follow the annotated
		// artifact's ADR-0007 link capability, so a valid id reads and an
		// unknown/expired id is a uniform 404.
		r.Get("/artifacts/{id}/reactions", s.handleListReactions)
		r.With(s.requireAuth, s.requireScope(scopeAnnotationsWrite), s.enforceCSRF).Post("/artifacts/{id}/reactions", s.handleReact)
		r.With(s.requireAuth, s.requireScope(scopeAnnotationsWrite), s.enforceCSRF).Delete("/artifacts/{id}/reactions", s.handleUnreact)
		r.With(s.requireAuth, s.requireScope(scopeAnnotationsWrite), s.enforceCSRF).Delete("/artifacts/{id}/reactions/{rid}", s.handleUnreactByID)
		r.Get("/artifacts/{id}/comments", s.handleListComments)
		r.With(s.requireAuth, s.requireScope(scopeAnnotationsWrite), s.enforceCSRF).Post("/artifacts/{id}/comments", s.handleComment)

		// The trajectory share type contributes its own /v1/runs* ingest,
		// lifecycle, and lazy-output surface through the ADR-0002 RouteMounter
		// capability. Its service is a runtime dependency (a live pool + object
		// store) the adapter co-constructs, so the adapter captures it in a
		// RouteMounter and mounts it through the same seam an out-of-tree share
		// type would use — the core router carries no trajectory-specific paths.
		if s.traj != nil {
			runMux{s: s}.MountRoutes(r)
		}

		// The webhook share type contributes its own /v1/hooks* management and
		// captured-request read surface through the same RouteMounter seam
		// (SPEC-0005). This is the model + management story: the public,
		// anonymous-write ingress that fills the buffer is deliberately not
		// mounted here (see hooks.go).
		if s.hook != nil {
			hookMux{s: s}.MountRoutes(r)
		}

		// Externally-registered share types contribute their own service
		// surfaces through the registry's RouteMounter capability too; a new
		// type's routes arrive by registration alone, with no edit to this
		// router (ADR-0002).
		s.reg.MountRoutes(r)
	})
}

// artifactResponse is the JSON view of an artifact. checksum is the SHA-256 a
// reader can re-verify (empty for bundles, which have no single body).
type artifactResponse struct {
	ID          string              `json:"id"`
	URL         string              `json:"url"`
	MCP         string              `json:"mcp"`
	ShareType   artifact.ShareType  `json:"share_type"`
	Title       string              `json:"title,omitempty"`
	Size        int64               `json:"size"`
	MediaType   string              `json:"media_type"`
	Previewable bool                `json:"previewable"`
	Checksum    string              `json:"checksum,omitempty"`
	Badge       string              `json:"badge"`
	Provenance  provenanceView      `json:"provenance"`
	Visibility  artifact.Visibility `json:"visibility"`
	// Tags are client-asserted routing strings, omitted when none were set. A
	// sibling of provenance, not a field inside it: nothing here is
	// server-derived, so nothing here is a trust signal (ADR-0018).
	Tags []string `json:"tags,omitempty"`
	// Denormalized annotation rollups, exposed separately — never summed —
	// so the Bin row (💬 2 · 👀 3) and headers (2 pins · 8 reactions) can
	// render each figure (SPEC-0006 REQ "Count Aggregation").
	ReactionCount int       `json:"reaction_count"`
	CommentCount  int       `json:"comment_count"`
	PinCount      int       `json:"pin_count"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	// ExpiresIn / ExpiresInSeconds are the server-computed TTL countdown
	// (issue #94, SPEC-0009 REQ "Default 7-Day TTL, Owner-Adjustable, Visible
	// Countdown": "remaining time MUST be surfaced as a visible countdown").
	// ExpiresIn is the humanized form the web shell already renders
	// ("in 6d"/"expired"); ExpiresInSeconds is the same remaining duration in
	// raw seconds (clamped to 0, never negative) for a scripted caller (CLI,
	// MCP, the owner-controls JS) to render its own countdown without
	// re-deriving it from expires_at against its own clock skew.
	ExpiresIn        string `json:"expires_in"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	// OwnerRedaction is the ingest scan outcome, set only for the owner and
	// omitted for everyone else (SPEC-0017 RD-9, see redaction.go).
	OwnerRedaction
}

type provenanceView struct {
	Actor      string           `json:"actor"`
	OnBehalfOf string           `json:"on_behalf_of,omitempty"`
	Channel    artifact.Channel `json:"channel"`
	CapturedAt time.Time        `json:"captured_at"`
}

func (s *Server) toArtifactResponse(a *artifact.Artifact) artifactResponse {
	return artifactResponse{
		ID:          a.PublicID,
		URL:         s.webURL(a),
		MCP:         s.mcpHandle(a),
		ShareType:   a.ShareType,
		Title:       a.Title,
		Size:        a.Size,
		MediaType:   a.MediaType,
		Previewable: a.Previewable,
		Checksum:    a.BodySHA256,
		Badge:       s.reg.BadgeFor(a),
		Provenance: provenanceView{
			Actor:      a.Provenance.ActorID,
			OnBehalfOf: a.Provenance.OnBehalfOf,
			Channel:    a.Provenance.Channel,
			CapturedAt: a.Provenance.CapturedAt,
		},
		Visibility:       a.Access.Visibility,
		Tags:             a.Tags,
		ReactionCount:    a.ReactionCount,
		CommentCount:     a.CommentCount,
		PinCount:         a.PinCount,
		CreatedAt:        a.CreatedAt,
		ExpiresAt:        a.ExpiresAt,
		ExpiresIn:        humanizeUntil(a.ExpiresAt),
		ExpiresInSeconds: secondsUntil(a.ExpiresAt),
	}
}

// webURL builds the human short URL. A type's legible sub-path (trajectory's
// /run/) is registry data via the URLPrefixer capability — never a switch on
// the type key here (ADR-0002; ADR-0005 URL scheme).
func (s *Server) webURL(a *artifact.Artifact) string {
	return s.cfg.BaseURL + prefixedPath(s.reg.URLPrefixFor(a.ShareType).Web, a.PublicID)
}

// mcpHandle builds the agent handle. Webhook's hook/ and trajectory's run/
// sub-prefixes come from the same registry capability; the same id token is
// reused verbatim (ADR-0005).
func (s *Server) mcpHandle(a *artifact.Artifact) string {
	return "mcp://cairn" + prefixedPath(s.reg.URLPrefixFor(a.ShareType).MCP, a.PublicID)
}

// prefixedPath joins an optional single-segment prefix and an id into a path.
func prefixedPath(prefix, id string) string {
	if prefix != "" {
		return "/" + prefix + "/" + id
	}
	return "/" + id
}
