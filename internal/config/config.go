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
	"os"
	"strconv"
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

	// BaseURL is the public origin used to build short URLs (ADR-0005),
	// e.g. https://cairn.sh.
	BaseURL string

	// Rate limiting for public/ingress endpoints (SPEC-0002 REQ "Rate
	// Limiting"): tokens per second and bucket burst, per client IP.
	RatePerSecond float64
	RateBurst     int

	// DevLoginPassword is the shared secret the MVP web login accepts for any
	// actor id (SPEC-0001, ADR-0004). Empty disables interactive login (the
	// deployment relies on bearer tokens only); real per-user auth is OAuth (#22).
	DevLoginPassword string

	// SessionTTL is the lifetime of a web session and its cookies (SPEC-0001).
	SessionTTL time.Duration

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
		BaseURL:     env("CAIRN_BASE_URL", "https://cairn.sh"),

		DevLoginPassword: os.Getenv("CAIRN_DEV_LOGIN_PASSWORD"),
		APITokensRaw:     os.Getenv("CAIRN_API_TOKENS"),
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
	return c, nil
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
