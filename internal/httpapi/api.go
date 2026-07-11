package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/sharetype"
	"github.com/joestump/cairn/internal/store"
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
}

// Server is the /v1 REST adapter over the core store.
type Server struct {
	store   *store.Store
	reg     *sharetype.Registry
	auth    Authenticator
	cfg     Config
	limiter *rateLimiter
	log     *slog.Logger
	now     func() time.Time
}

// New constructs a Server. If auth is nil a BearerAuthenticator is used; if
// logger is nil slog.Default() is used.
func New(st *store.Store, reg *sharetype.Registry, auth Authenticator, cfg Config, logger *slog.Logger) *Server {
	if auth == nil {
		auth = BearerAuthenticator{}
	}
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
	return &Server{
		store:   st,
		reg:     reg,
		auth:    auth,
		cfg:     cfg,
		limiter: newRateLimiter(cfg.RatePerSecond, cfg.RateBurst),
		log:     logger,
		now:     time.Now,
	}
}

// Handler returns the routed http.Handler for the /v1 surface.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(s.securityHeaders)
	r.Use(s.rateLimit)

	r.Route("/v1", func(r chi.Router) {
		// Mutating / workspace-scoped endpoints require authentication.
		r.With(s.requireAuth).Post("/artifacts", s.handleCreate)
		r.With(s.requireAuth).Delete("/artifacts/{id}", s.handleDelete)
		r.With(s.requireAuth).Get("/bin", s.handleBin)

		// Link-capability reads: a valid id grants read; unknown/expired ids
		// return a uniform 404 (ADR-0007).
		r.Get("/artifacts/{id}", s.handleGet)
		r.Get("/artifacts/{id}/body", s.handleGetBody)
		r.Get("/artifacts/{id}/members/*", s.handleGetMember)

		// Share types contribute their own service surfaces (e.g. trajectory's
		// /v1/runs* ingest routes) through the registry's RouteMounter
		// capability; a new type's routes arrive by registration alone, with no
		// edit to this router (ADR-0002).
		s.reg.MountRoutes(r)
	})
	return r
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
