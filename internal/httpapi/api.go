package httpapi

import (
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/joestump/cairn/internal/annotation"
	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/session"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/store"
	"github.com/joestump/cairn/internal/trajectory"
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
	// ADR-0004) accepts for any actor id. An empty value disables interactive web
	// login entirely (the deployment opted out), so login fails closed. Real
	// per-user auth replaces this with OAuth (#22).
	DevLoginPassword string
	// SessionTTL is the lifetime of a web session and its cookies. Defaults to 7
	// days.
	SessionTTL time.Duration
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
}

// Server is the /v1 REST adapter over the core store and the ADR-0011 web app
// shell. Both are thin projections of the same core (ADR-0003): the JSON API and
// the HTML shell share the store, registry, and annotation service, so they can
// never disagree about an artifact's facts.
type Server struct {
	store   *store.Store
	annot   *annotation.Service
	traj    *trajectory.Service
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
	if cfg.MaxRunRequestBytes <= 0 {
		cfg.MaxRunRequestBytes = 64 << 20
	}
	if cfg.StreamHeartbeat <= 0 {
		cfg.StreamHeartbeat = 15 * time.Second
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	// The annotation and trajectory cores are peers of the artifact store,
	// projected by this same adapter (SPEC-0006 REQ "Cross-Surface Parity",
	// SPEC-0004). Each shares the store's pool and registry so its writes land in
	// the same database and transaction domain and gate anchors against the same
	// capability matrix (ADR-0012 one binary, one core); the trajectory service
	// additionally spills oversized span outputs to the store's object store. A
	// nil store (unit tests that exercise only URL/auth helpers) leaves both nil.
	var (
		annot    *annotation.Service
		traj     *trajectory.Service
		sessions session.Store
	)
	if st != nil {
		annot = annotation.NewService(st.Pool(), reg)
		traj = trajectory.NewService(st.Pool(), st.ObjectStore(), trajectory.Options{Registry: reg})
		// The web session store lives in the same Postgres as the core, so the
		// single binary carries its schema and a scaled deployment shares one
		// session table (ADR-0012).
		sessions = session.NewPostgresStore(st.Pool())
	}
	// Auth seam (ADR-0004): a caller-supplied Authenticator wins; otherwise build
	// the bearer surface from configured static tokens (the verifying
	// TokenAuthenticator — a raw bearer is never trusted as an actor), optionally
	// chained with the INSECURE dev shortcut when explicitly enabled. The bearer
	// surface backs both the standalone API wiring and the session-aware adapter,
	// so the web binary authenticates browser sessions AND verified bearer tokens.
	if auth == nil {
		var bearer Authenticator = NewTokenAuthenticator(cfg.APITokens)
		if cfg.DevInsecureBearerAuth {
			bearer = chainAuthenticator{bearer, DevActorAuthenticator{}}
		}
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

	// The /v1 REST/JSON API, under the strict API CSP.
	r.Group(func(r chi.Router) {
		r.Use(s.securityHeaders)
		s.mountAPI(r)
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
