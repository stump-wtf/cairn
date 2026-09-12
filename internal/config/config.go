// Package config loads Cairn's runtime configuration from the environment.
//
// The single static binary reads all configuration from the environment
// (ADR-0012 "Deployment shape"): a Postgres DSN for metadata and an
// S3-compatible endpoint for content-addressed bodies (ADR-0008).
//
// Governing: ADR-0012 (Backend Platform and API Shape), ADR-0008 (Storage & Content Model)
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	HTTPAddr string

	// DatabaseURL is the Postgres DSN holding all queryable metadata.
	DatabaseURL string

	// S3-compatible object storage for body blobs (ADR-0008).
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3Region    string
	S3UseSSL    bool

	// MaxUploadBytes caps a single streamed body; enforced incrementally
	// during ingest so an oversize upload is rejected mid-stream (413).
	MaxUploadBytes int64

	// PreviewMaxBytes is the upper bound for a body to be considered for a
	// rich viewer; bodies above it fall back to the generic file path.
	PreviewMaxBytes int64

	// DefaultTTL is the default artifact expiry when a surface does not set
	// one explicitly (ADR-0007 short default TTL).
	DefaultTTL time.Duration

	// Retention reaper (SPEC-0009 REQ "Hard Delete on Expiry via
	// Reference-Counted Reaper", REQ "Concurrency Safety (Expiry Reaper)").
	// ReapInterval is the ticker period of the background reaper worker in
	// cairnd; ReapBatch bounds each phase's per-cycle work; ReapObjectGrace is
	// the minimum age a blobs/ object must reach before the orphan-object scan
	// (the no-DB-row backstop) may delete it — defense-in-depth around the brief
	// window between a create promoting its object and committing its blob row.
	ReapInterval    time.Duration
	ReapBatch       int
	ReapObjectGrace time.Duration

	// StagingLifecycleTTL sets the S3 lifecycle expiration on the staging/
	// prefix so abandoned upload debris is reaped even if a process crashed
	// mid-upload. It is scoped to staging/ ONLY and MUST NEVER be applied to the
	// committed blobs/ prefix (content-addressed dedup means an object's age has
	// no relationship to its longest live reference — issue #93 §1, SPEC-0009).
	StagingLifecycleTTL time.Duration

	// BaseURL is the public origin used to build short URLs (ADR-0005),
	// e.g. https://cairn.stump.wtf.
	BaseURL string

	// Rate limiting for public/ingress endpoints (SPEC-0002 REQ "Rate
	// Limiting"): tokens per second and bucket burst, per client IP.
	RatePerSecond float64
	RateBurst     int

	// DevLoginPassword is the shared secret the MVP web login accepts for any
	// actor id (SPEC-0001, ADR-0004). Real human auth is native OIDC against
	// Pocket ID (ADR-0013); this fallback is demoted to local-dev-only — it is
	// honored only when CAIRN_OIDC_ISSUER is unset, so a production deployment
	// that configures OIDC can never fall back to it. Empty disables interactive
	// login entirely (the deployment relies on bearer tokens only).
	DevLoginPassword string

	// SessionTTL is the lifetime of a web session and its cookies (SPEC-0001).
	SessionTTL time.Duration

	// OIDC relying-party config (ADR-0013): Cairn logs the human in directly
	// against Pocket ID rather than fronting itself with oauth2-proxy. OIDC is
	// "enabled" iff OIDCIssuer is non-empty (OIDCConfigured); the redirect URI is
	// always derived as BaseURL + /auth/callback, never separately configured, so
	// it can never drift from the public origin Pocket ID was registered against.
	OIDCIssuer       string // e.g. https://pocket-id.stump.rocks
	OIDCClientID     string // defaults to "cairn"
	OIDCClientSecret string

	// Outbound webhooks (ADR-0017, SPEC-0012): comma-separated target URLs
	// that receive a signed `artifact.created` event after every durable
	// artifact creation, and an optional HMAC secret for the
	// X-Cairn-Signature header. Empty URL list = the feature is inert.
	OutboundWebhookURLs   []string
	OutboundWebhookSecret string

	// OAuth 2.1 authorization-server tuning (SPEC-0007, ADR-0004):
	// access-token lifetime (~1h default), rotating refresh-token lifetime
	// (30d default), and the dedicated per-IP rate limit on the OAuth
	// bootstrap endpoints (register/token/revoke/authorize).
	OAuthAccessTokenTTL  time.Duration
	OAuthRefreshTokenTTL time.Duration
	OAuthRatePerSecond   float64
	OAuthRateBurst       int

	// Webhook open ingress (SPEC-0005, ADR-0010, issue #84): the
	// anonymous-write `ANY /h/{id}` route carries its OWN dedicated
	// per-source-IP and per-endpoint rate limiters, distinct from the
	// general RatePerSecond/RateBurst above and from OAuth's — the single
	// most exposed surface in Cairn must never share a budget with
	// authenticated traffic (SPEC-0005 REQ "Rate Limiting": "per-endpoint
	// and per-source-IP").
	HookIngressRatePerSecond  float64
	HookIngressRateBurst      int
	HookEndpointRatePerSecond float64
	HookEndpointRateBurst     int

	// APITokensRaw is the unparsed CAIRN_API_TOKENS value: a comma-separated list
	// of `secret:actor[:role]` static bearer credentials the API/MCP surface
	// accepts (ADR-0004 MVP token seam). Parsed and validated at startup; an empty
	// value means the bearer surface accepts no tokens. Real per-agent OAuth
	// tokens replace this in #22.
	APITokensRaw string

	// DevInsecureBearerAuth, when true, makes the API trust a raw bearer token AS
	// the actor id with no verification. It is an INSECURE local-development
	// shortcut that must never be enabled in production; the default is false, so
	// production verifies every bearer token against APITokens.
	DevInsecureBearerAuth bool
}

// Load resolves configuration from the environment, applying defaults and
// returning a descriptive error for any malformed value.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    env("CAIRN_HTTP_ADDR", ":8080"),
		DatabaseURL: env("CAIRN_DATABASE_URL", os.Getenv("DATABASE_URL")),
		S3Endpoint:  env("CAIRN_S3_ENDPOINT", "localhost:9000"),
		S3AccessKey: env("CAIRN_S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey: env("CAIRN_S3_SECRET_KEY", "minioadmin"),
		S3Bucket:    env("CAIRN_S3_BUCKET", "cairn"),
		S3Region:    env("CAIRN_S3_REGION", "us-east-1"),
		BaseURL:     env("CAIRN_BASE_URL", "https://cairn.stump.wtf"),

		DevLoginPassword: os.Getenv("CAIRN_DEV_LOGIN_PASSWORD"),
		APITokensRaw:     os.Getenv("CAIRN_API_TOKENS"),

		OIDCIssuer:       os.Getenv("CAIRN_OIDC_ISSUER"),
		OIDCClientID:     env("CAIRN_OIDC_CLIENT_ID", "cairn"),
		OIDCClientSecret: os.Getenv("CAIRN_OIDC_CLIENT_SECRET"),

		OutboundWebhookSecret: os.Getenv("CAIRN_OUTBOUND_WEBHOOK_SECRET"),
	}
	// Governing: SPEC-0012 REQ "Delivery Targets from Configuration".
	for _, raw := range strings.Split(os.Getenv("CAIRN_OUTBOUND_WEBHOOK_URLS"), ",") {
		if u := strings.TrimSpace(raw); u != "" {
			c.OutboundWebhookURLs = append(c.OutboundWebhookURLs, u)
		}
	}
	if err := validateWebhookURLs(c.OutboundWebhookURLs); err != nil {
		return nil, err
	}

	var err error
	if c.DevInsecureBearerAuth, err = envBool("CAIRN_DEV_INSECURE_BEARER_AUTH", false); err != nil {
		return nil, err
	}
	if c.S3UseSSL, err = envBool("CAIRN_S3_USE_SSL", false); err != nil {
		return nil, err
	}
	if c.MaxUploadBytes, err = envInt64("CAIRN_MAX_UPLOAD_BYTES", 64<<20); err != nil {
		return nil, err
	}
	if c.PreviewMaxBytes, err = envInt64("CAIRN_PREVIEW_MAX_BYTES", 5<<20); err != nil {
		return nil, err
	}
	if c.DefaultTTL, err = envDuration("CAIRN_DEFAULT_TTL", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.ReapInterval, err = envDuration("CAIRN_REAP_INTERVAL", time.Hour); err != nil {
		return nil, err
	}
	var reapBatch int64
	if reapBatch, err = envInt64("CAIRN_REAP_BATCH", 500); err != nil {
		return nil, err
	}
	c.ReapBatch = int(reapBatch)
	if c.ReapObjectGrace, err = envDuration("CAIRN_REAP_OBJECT_GRACE", time.Hour); err != nil {
		return nil, err
	}
	if c.StagingLifecycleTTL, err = envDuration("CAIRN_STAGING_LIFECYCLE_TTL", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.RatePerSecond, err = envFloat("CAIRN_RATE_PER_SECOND", 20); err != nil {
		return nil, err
	}
	var burst int64
	if burst, err = envInt64("CAIRN_RATE_BURST", 40); err != nil {
		return nil, err
	}
	c.RateBurst = int(burst)
	if c.SessionTTL, err = envDuration("CAIRN_SESSION_TTL", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.OAuthAccessTokenTTL, err = envDuration("CAIRN_OAUTH_ACCESS_TTL", time.Hour); err != nil {
		return nil, err
	}
	if c.OAuthRefreshTokenTTL, err = envDuration("CAIRN_OAUTH_REFRESH_TTL", 30*24*time.Hour); err != nil {
		return nil, err
	}
	if c.OAuthRatePerSecond, err = envFloat("CAIRN_OAUTH_RATE_PER_SECOND", 10); err != nil {
		return nil, err
	}
	var oauthBurst int64
	if oauthBurst, err = envInt64("CAIRN_OAUTH_RATE_BURST", 30); err != nil {
		return nil, err
	}
	c.OAuthRateBurst = int(oauthBurst)
	if c.HookIngressRatePerSecond, err = envFloat("CAIRN_HOOK_INGRESS_RATE_PER_SECOND", 5); err != nil {
		return nil, err
	}
	var hookIngressBurst int64
	if hookIngressBurst, err = envInt64("CAIRN_HOOK_INGRESS_RATE_BURST", 20); err != nil {
		return nil, err
	}
	c.HookIngressRateBurst = int(hookIngressBurst)
	if c.HookEndpointRatePerSecond, err = envFloat("CAIRN_HOOK_ENDPOINT_RATE_PER_SECOND", 10); err != nil {
		return nil, err
	}
	var hookEndpointBurst int64
	if hookEndpointBurst, err = envInt64("CAIRN_HOOK_ENDPOINT_RATE_BURST", 50); err != nil {
		return nil, err
	}
	c.HookEndpointRateBurst = int(hookEndpointBurst)
	return c, nil
}

// OIDCConfigured reports whether the OIDC relying-party settings are present —
// the single gate (ADR-0013) that decides whether Cairn logs humans in via
// Pocket ID (this true) or falls back to dev_login_password (this false).
func (c *Config) OIDCConfigured() bool {
	return c.OIDCIssuer != ""
}

func envFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config %s: %w", key, err)
	}
	return f, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config %s: %w", key, err)
	}
	return b, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config %s: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config %s: %w", key, err)
	}
	return d, nil
}

// validateWebhookURLs enforces the SPEC-0012 Security Requirements rule that
// outbound webhook targets must be https:// unless they are loopback hosts,
// where plaintext is tolerated for local development. A malformed URL fails
// startup rather than silently becoming a delivery that can never succeed.
//
// @joestump-agent 09/06/2026 - Added during review of #175: the spec required
// TLS for non-localhost targets but nothing enforced it.
func validateWebhookURLs(urls []string) error {
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("config CAIRN_OUTBOUND_WEBHOOK_URLS: %q: %w", raw, err)
		}
		switch u.Scheme {
		case "https":
		case "http":
			if u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
				return fmt.Errorf("config CAIRN_OUTBOUND_WEBHOOK_URLS: %q: non-localhost targets require https (SPEC-0012)", raw)
			}
		default:
			return fmt.Errorf("config CAIRN_OUTBOUND_WEBHOOK_URLS: %q: scheme must be http(s)", raw)
		}
	}
	return nil
}
