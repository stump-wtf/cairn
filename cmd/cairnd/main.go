// Command cairnd is the single static Cairn binary: it serves the web app, the
// /v1 REST/JSON API, the SSE streams, and the MCP server over one core package
// (ADR-0012). This entry point wires configuration, the Postgres pool, embedded
// schema migrations, and the object store, then serves HTTP.
//
// The /v1 artifact routes land in SPEC-0002 story #10; this scaffold stands up
// the process, health check, and dependency wiring the rest of the API hangs on.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/joestump/cairn/internal/config"
	"github.com/joestump/cairn/internal/db"
	"github.com/joestump/cairn/internal/httpapi"
	"github.com/joestump/cairn/internal/objectstore"
	"github.com/joestump/cairn/internal/outboundhook"
	"github.com/joestump/cairn/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("cairnd exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	obj, err := objectstore.NewMinIO(ctx, objectstore.MinIOConfig{
		Endpoint:  cfg.S3Endpoint,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
		Bucket:    cfg.S3Bucket,
		Region:    cfg.S3Region,
		UseSSL:    cfg.S3UseSSL,
	})
	if err != nil {
		return err
	}

	// Outbound webhook emitter (ADR-0017, SPEC-0012): inert unless target URLs
	// are configured. One hook at the store choke point covers every creation
	// surface (REST/web/CLI/MCP); delivery runs on its own worker goroutine,
	// reaper-style, and stops cleanly on shutdown.
	var emitter *outboundhook.Emitter
	if len(cfg.OutboundWebhookURLs) > 0 {
		emitter = outboundhook.New(cfg.OutboundWebhookURLs, cfg.OutboundWebhookSecret, cfg.BaseURL, logger)
		logger.Info("outbound webhooks enabled", "targets", len(cfg.OutboundWebhookURLs), "signed", cfg.OutboundWebhookSecret != "")
	}

	// The core service the transport adapters (REST/MCP/CLI) project. The
	// share-type registry (previewability, anchor affordances) defaults to the
	// process-wide sharetype.Default().
	svc := store.New(pool, obj, store.Options{
		MaxUploadBytes:  cfg.MaxUploadBytes,
		PreviewMaxBytes: cfg.PreviewMaxBytes,
		Emitter:         emitter,
	})

	// Install the staging/ debris lifecycle rule (best-effort defense-in-depth;
	// scoped to staging/ ONLY — never the committed blobs/ prefix, issue #93 §1).
	// A failure here is logged, not fatal: the ingest paths already reclaim their
	// staging objects, so this rule only mops up crash debris.
	if err := obj.EnsureStagingLifecycle(ctx, cfg.StagingLifecycleTTL); err != nil {
		logger.Warn("could not install staging lifecycle rule (non-fatal)", "error", err)
	}

	// The background retention reaper (SPEC-0009): hard-deletes expired artifacts
	// and reference-count-GCs their orphaned content-addressed blobs, with an
	// object-storage orphan-scan backstop. It runs for the life of the process
	// and stops cleanly when ctx is cancelled (graceful shutdown).
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		svc.RunReaper(ctx, cfg.ReapInterval, store.ReaperConfig{
			Batch:       cfg.ReapBatch,
			ObjectGrace: cfg.ReapObjectGrace,
		}, logger)
	}()

	// Outbound webhook delivery worker: same lifecycle contract as the reaper
	// (SPEC-0012 REQ "Graceful Lifecycle").
	hookDone := make(chan struct{})
	go func() {
		defer close(hookDone)
		if emitter != nil {
			emitter.Run(ctx)
		}
	}()

	// Parse the static API bearer credentials (ADR-0004 MVP token seam) at
	// startup so a malformed CAIRN_API_TOKENS fails the process rather than
	// silently dropping a credential. A raw bearer token is only ever trusted when
	// it verifies against this set (or, in dev, when the insecure shortcut is on).
	apiTokens, err := httpapi.ParseAPITokens(cfg.APITokensRaw)
	if err != nil {
		return err
	}
	if cfg.DevInsecureBearerAuth {
		logger.Warn("CAIRN_DEV_INSECURE_BEARER_AUTH is enabled: raw bearer tokens are trusted as actor ids without verification — never enable this in production")
	} else if len(apiTokens) == 0 && cfg.DevLoginPassword == "" && !cfg.OIDCConfigured() {
		logger.Warn("no API tokens (CAIRN_API_TOKENS), no OIDC (CAIRN_OIDC_ISSUER), and no dev web login (CAIRN_DEV_LOGIN_PASSWORD) configured: all authenticated endpoints will reject every caller")
	}
	if cfg.OIDCConfigured() && cfg.DevLoginPassword != "" {
		logger.Warn("CAIRN_OIDC_ISSUER and CAIRN_DEV_LOGIN_PASSWORD are both set: OIDC wins — the dev-password login is disabled while OIDC is configured (ADR-0013)")
	}

	// The /v1 REST/JSON adapter over the core service (ADR-0012).
	api := httpapi.New(svc, nil, nil, httpapi.Config{
		BaseURL:               cfg.BaseURL,
		MaxUploadBytes:        cfg.MaxUploadBytes,
		DefaultTTL:            cfg.DefaultTTL,
		RatePerSecond:         cfg.RatePerSecond,
		RateBurst:             cfg.RateBurst,
		DevLoginPassword:      cfg.DevLoginPassword,
		SessionTTL:            cfg.SessionTTL,
		OIDCIssuer:            cfg.OIDCIssuer,
		OIDCClientID:          cfg.OIDCClientID,
		OIDCClientSecret:      cfg.OIDCClientSecret,
		APITokens:             apiTokens,
		DevInsecureBearerAuth: cfg.DevInsecureBearerAuth,
		AccessTokenTTL:        cfg.OAuthAccessTokenTTL,
		RefreshTokenTTL:       cfg.OAuthRefreshTokenTTL,
		OAuthRatePerSecond:    cfg.OAuthRatePerSecond,
		OAuthRateBurst:        cfg.OAuthRateBurst,

		HookIngressRatePerSecond:  cfg.HookIngressRatePerSecond,
		HookIngressRateBurst:      cfg.HookIngressRateBurst,
		HookEndpointRatePerSecond: cfg.HookEndpointRatePerSecond,
		HookEndpointRateBurst:     cfg.HookEndpointRateBurst,
	}, logger)

	// Discover the OIDC issuer and wire the "Sign in with Pocket ID" relying
	// party (ADR-0013). A no-op when CAIRN_OIDC_ISSUER is unset; a discovery
	// failure is fatal — never start serving with human login silently broken.
	if err := api.EnableOIDC(ctx); err != nil {
		return err
	}
	if cfg.OIDCConfigured() {
		logger.Info("OIDC login enabled", "issuer", cfg.OIDCIssuer, "client_id", cfg.OIDCClientID)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Mount("/", api.Handler())

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("cairnd listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		// ctx is already cancelled (that is what woke us); wait for the reaper to
		// unwind its current sweep so shutdown is clean (SPEC-0009 REQ "Concurrency
		// Safety (Expiry Reaper)": graceful shutdown, no orphaned goroutine).
		<-reaperDone
		<-hookDone
		return err
	case err := <-errCh:
		return err
	}
}
