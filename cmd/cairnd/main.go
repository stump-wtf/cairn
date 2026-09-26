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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/annotation"
	"github.com/stump-wtf/cairn/internal/config"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/httpapi"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("cairnd exited", "error", err)
		os.Exit(1)
	}
}

// newOutboundEmitter builds the outbound-webhook emitter, or returns nil when
// no target URLs are configured (ADR-0017, SPEC-0012: inert unless configured).
//
// It returns the CONCRETE *outboundhook.Emitter rather than the
// store.CreationEmitter interface, deliberately: the delivery worker in run()
// calls Run(ctx), which the interface does not declare. The typed-nil hazard
// that caused cairn#201 is handled where it actually arises — at the
// store.Options boundary, where the concrete pointer enters an interface field.
//
// Extracted so main()'s wiring decision is reachable from a test. Every other
// test in this repo constructs its own emitter, which is why none of them could
// see that main() was building a broken one.
func newOutboundEmitter(cfg *config.Config, logger *slog.Logger) *outboundhook.Emitter {
	if len(cfg.OutboundWebhookURLs) == 0 {
		return nil
	}
	logger.Info("outbound webhooks enabled",
		"targets", len(cfg.OutboundWebhookURLs),
		"signed", cfg.OutboundWebhookSecret != "")
	return outboundhook.New(cfg.OutboundWebhookURLs, cfg.OutboundWebhookSecret, cfg.BaseURL, logger)
}

// newStoreOptions assembles the store options, assigning the Emitter interface
// field ONLY when an emitter actually exists.
//
// This is the seam cairn#201 lived in. It previously read `Emitter: emitter`
// unconditionally, and store.Options.Emitter is an INTERFACE
// (store.CreationEmitter) while emitter is a *outboundhook.Emitter. An
// unconfigured deployment therefore handed over a non-nil interface wrapping a
// nil pointer: store's `s.emitter == nil` guard read false and every artifact
// create dispatched to a nil receiver. The panic landed AFTER the commit, so
// the row and blob were written and the caller still got a 500.
//
// Note the asymmetry with the `emitter != nil` check in the delivery worker:
// that one compares a concrete pointer and means what it looks like. This one
// would not, because the destination is an interface. Do not "unify" them.
//
// Extracted from run() so the decision is reachable from a test. Inline, it was
// not, which is why reverting the fix left cmd/cairnd's tests green.
func newStoreOptions(cfg *config.Config, emitter *outboundhook.Emitter) store.Options {
	opts := store.Options{
		MaxUploadBytes:  cfg.MaxUploadBytes,
		PreviewMaxBytes: cfg.PreviewMaxBytes,
	}
	if emitter != nil {
		opts.Emitter = emitter
	}
	return opts
}

// newEventEmitter returns the lifecycle-event emitter the core services beyond
// the store receive (ADR-0022, SPEC-0016 EV-2), as a nil INTERFACE when no
// outbound emitter exists. The same typed-nil hazard as newStoreOptions
// (cairn#201) applies: httpapi.Config.Events and trajectory.Options.Emitter are
// event.Emitter interfaces, so assigning a nil *outboundhook.Emitter would make
// every "is an emitter installed?" guard read true.
func newEventEmitter(emitter *outboundhook.Emitter) event.Emitter {
	if emitter == nil {
		return nil
	}
	return emitter
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
	// Runs are minted by the trajectory service, and annotations written by the
	// annotation service, not the store, so both receive the same emitter
	// through httpapi.Config.Events (SPEC-0016 EV-2).
	// Kept as the concrete *outboundhook.Emitter, not store.CreationEmitter:
	// the delivery worker below calls Run, which the interface does not declare.
	emitter := newOutboundEmitter(cfg, logger)

	// The core service the transport adapters (REST/MCP/CLI) project. The
	// share-type registry (previewability, anchor affordances) defaults to the
	// process-wide sharetype.Default().
	svc := store.New(pool, obj, newStoreOptions(cfg, emitter))

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
	// The EV-5 approval class, parsed at startup for the same reason: a typo in
	// CAIRN_APPROVAL_REACTIONS fails the process instead of silently making
	// every approval-class reaction a non-approval.
	approvalClass, err := annotation.NewApprovalClass(cfg.ApprovalReactions)
	if err != nil {
		return fmt.Errorf("config CAIRN_APPROVAL_REACTIONS: %w", err)
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
		Events:                newEventEmitter(emitter),
		ApprovalClass:         approvalClass,
		MaxUploadBytes:        cfg.MaxUploadBytes,
		DefaultTTL:            cfg.DefaultTTL,
		RatePerSecond:         cfg.RatePerSecond,
		RateBurst:             cfg.RateBurst,
		DevLoginPassword:      cfg.DevLoginPassword,
		SessionTTL:            cfg.SessionTTL,
		OIDCIssuer:            cfg.OIDCIssuer,
		OIDCClientID:          cfg.OIDCClientID,
		OIDCClientSecret:      cfg.OIDCClientSecret,
		GitHubClientID:        cfg.GitHubClientID,
		GitHubClientSecret:    cfg.GitHubClientSecret,
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
	// Wire the GitHub provider (SPEC-0012): a no-op unless
	// CAIRN_GITHUB_CLIENT_ID/SECRET are both set. Nothing here can fail —
	// GitHub needs no issuer discovery — so misconfiguration surfaces as a
	// 404 at the routes, not a startup error.
	api.EnableGitHub()
	if cfg.OIDCConfigured() {
		logger.Info("OIDC login enabled", "issuer", cfg.OIDCIssuer, "client_id", cfg.OIDCClientID)
	}
	if cfg.GitHubConfigured() {
		logger.Info("GitHub login enabled", "client_id", cfg.GitHubClientID)
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
