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

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/mcpsession"
	"github.com/joestump/cairn/internal/oauth"
	"github.com/joestump/cairn/internal/pat"
	"github.com/joestump/cairn/internal/session"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/store"
	"github.com/joestump/cairn/internal/trajectory"
	"github.com/joestump/cairn/internal/webhook"
)

// Config tunes the REST adapter.
type Config struct {
	// BaseURL is the public origin used to build short URLs, e.g.
	// https://cairn.sh. No trailing slash.
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
	// OAuth 2.1 authorization server (SPEC-0007, ADR-0004): the in-process core
	// service the /oauth endpoints adapt, plus its dedicated tighter rate
	// limiter. Nil on storeless unit wirings (the AS persists in Postgres).
	oauth        *oauth.Service
	oauthLimiter *rateLimiter
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
		cfg.BaseURL = "https://cairn.sh"
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
		annot = annotation.NewService(st.Pool(), reg)
		traj = trajectory.NewService(st.Pool(), st.ObjectStore(), trajectory.Options{Registry: reg})
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
	return &Server{
		store:         st,
		annot:         annot,
		traj:          traj,
		hook:          hookSvc,
		reg:           reg,
		auth:          auth,
		cfg:           cfg,
		limiter:       newRateLimiter(cfg.RatePerSecond, cfg.RateBurst),
		log:           logger,
		now:           time.Now,
		webTmpl:       parseWebTemplates(),
		sessions:      sessions,
		verifier:      DevPasswordVerifier{Password: cfg.DevLoginPassword},
		secureCookies: strings.HasPrefix(cfg.BaseURL, "https://"),
		oauth:         oauthSvc,
		oauthLimiter:  newRateLimiter(cfg.OAuthRatePerSecond, cfg.OAuthRateBurst),
		pat:           patSvc,
		mcpSessions:   mcpSessSvc,
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
	// Denormalized annotation rollups, exposed separately — never summed —
	// so the Bin row (💬 2 · 👀 3) and headers (2 pins · 8 reactions) can
	// render each figure (SPEC-0006 REQ "Count Aggregation").
	ReactionCount int       `json:"reaction_count"`
	CommentCount  int       `json:"comment_count"`
	PinCount      int       `json:"pin_count"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
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
		Visibility:    a.Access.Visibility,
		ReactionCount: a.ReactionCount,
		CommentCount:  a.CommentCount,
		PinCount:      a.PinCount,
		CreatedAt:     a.CreatedAt,
		ExpiresAt:     a.ExpiresAt,
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
