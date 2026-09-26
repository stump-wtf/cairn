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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/config"
	"github.com/stump-wtf/cairn/internal/db"
	"github.com/stump-wtf/cairn/internal/httpapi"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/operator"
	"github.com/stump-wtf/cairn/internal/outboundhook"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/subscription"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("cairnd exited", "error", err)
		os.Exit(1)
	}
}

// newSubscriptions builds the owned outbound subscription core (ADR-0029
// section 6, SPEC-0023 REQ "Owned Outbound Subscriptions"). A malformed
// CAIRN_ENCRYPTION_KEY fails boot; an unset one leaves subscriptions
// uncreatable and says so, and the server runs normally. The risky
// CAIRN_OUTBOUND_ALLOW_HTTP is loud when on (REQ "Subscription Target
// Safety"). Neither the key nor any target is logged.
func newSubscriptions(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (*subscription.Service, error) {
	sealer, err := subscription.ParseKey(cfg.EncryptionKeyRaw)
	if err != nil {
		return nil, err
	}
	if sealer == nil {
		logger.Info("outbound subscriptions unavailable: " + subscription.KeyEnv + " is unset, so no subscription can be created or rotated")
	}
	if cfg.OutboundAllowHTTP {
		logger.Warn(subscription.AllowHTTPEnv + " is enabled: subscriptions may deliver signed events and capability URLs over plaintext http — never enable this in production")
	}
	return subscription.NewService(pool, subscription.Options{
		Sealer:  sealer,
		Policy:  newSubscriptionPolicy(cfg),
		PerUser: cfg.SubscriptionsPerUser,
		PerTeam: cfg.SubscriptionsPerTeam,
	}), nil
}

// newSubscriptionPolicy is the production target policy: https only unless
// CAIRN_OUTBOUND_ALLOW_HTTP, public addresses only, the system resolver. It
// never sets Policy.PermitAddr, which exists for tests; a test pins that.
func newSubscriptionPolicy(cfg *config.Config) *subscription.Policy {
	return &subscription.Policy{AllowHTTP: cfg.OutboundAllowHTTP}
}

// newOutboundEmitter builds the outbound emitter over the subscription core.
// There is no instance-wide target list, so it always exists: with no
// subscriptions it looks up nothing to deliver to.
//
// It returns the CONCRETE *outboundhook.Emitter rather than the
// store.CreationEmitter interface, deliberately: the delivery worker in run()
// calls Run(ctx), which the interface does not declare. The typed-nil hazard
// that caused cairn#201 is handled where it actually arises — at the
// store.Options boundary, where the concrete pointer enters an interface field.
func newOutboundEmitter(cfg *config.Config, subs *subscription.Service, logger *slog.Logger) *outboundhook.Emitter {
	return outboundhook.New(subs, subs.Policy(), cfg.BaseURL, logger)
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

	// Outbound delivery (ADR-0017, SPEC-0012, ADR-0029 section 6): events go
	// to the owned subscriptions of the workspace that owns the artifact. One
	// hook at the store choke point covers every creation surface
	// (REST/web/CLI/MCP); delivery runs on its own workers, reaper-style, and
	// stops cleanly on shutdown.
	// Kept as the concrete *outboundhook.Emitter, not store.CreationEmitter:
	// the delivery worker below calls Run, which the interface does not declare.
	subs, err := newSubscriptions(cfg, pool, logger)
	if err != nil {
		return err
	}
	emitter := newOutboundEmitter(cfg, subs, logger)

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
	// startup so a malformed or legacy CAIRN_API_TOKENS entry fails the process
	// rather than silently dropping a credential. A raw bearer token is only
	// ever trusted when it verifies against this set once ResolveAPITokens has
	// bound it below (or, in dev, when the insecure shortcut is on).
	apiTokens, err := httpapi.ParseAPITokens(cfg.APITokensRaw)
	if err != nil {
		return err
	}
	if cfg.DevInsecureBearerAuth {
		logger.Warn("CAIRN_DEV_INSECURE_BEARER_AUTH is enabled: raw bearer tokens are trusted as actor ids without verification — never enable this in production")
	} else if len(apiTokens) == 0 && cfg.DevLoginPassword == "" && !cfg.OIDCConfigured() {
		logger.Warn("no API tokens (CAIRN_API_TOKENS), no OIDC (CAIRN_OIDC_ISSUER), and no dev web login (CAIRN_DEV_LOGIN_PASSWORD) configured: all authenticated endpoints will reject every caller")
	}
	// The operator profile (SPEC-0023 REQ "Operator and User Profiles"). A
	// malformed CAIRN_OPERATORS entry fails boot rather than silently
	// dropping an operator. The identities themselves are not logged.
	operators, err := operator.Parse(cfg.OperatorsRaw, cfg.OperatorGroup)
	if err != nil {
		return err
	}
	if operators.Enabled() {
		logger.Info("operator profile enabled", "operators", operators.Len(), "operator_group_set", operators.Group() != "")
		if operators.Group() != "" && !cfg.OIDCConfigured() {
			logger.Warn("CAIRN_OPERATOR_GROUP is set but OIDC is not configured: the group is read from the OIDC groups claim, so no session can match it")
		}
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
		GitHubClientID:        cfg.GitHubClientID,
		GitHubClientSecret:    cfg.GitHubClientSecret,
		APITokens:             apiTokens,
		DevInsecureBearerAuth: cfg.DevInsecureBearerAuth,
		Operators:             operators,
		Subscriptions:         subs,
		AccessTokenTTL:        cfg.OAuthAccessTokenTTL,
		RefreshTokenTTL:       cfg.OAuthRefreshTokenTTL,
		OAuthRatePerSecond:    cfg.OAuthRatePerSecond,
		OAuthRateBurst:        cfg.OAuthRateBurst,

		HookIngressRatePerSecond:  cfg.HookIngressRatePerSecond,
		HookIngressRateBurst:      cfg.HookIngressRateBurst,
		HookEndpointRatePerSecond: cfg.HookEndpointRatePerSecond,
		HookEndpointRateBurst:     cfg.HookEndpointRateBurst,
	}, logger)

	// Bind each CAIRN_API_TOKENS entry to the operator's user it names
	// (SPEC-0023 REQ "Static API Tokens Act as an Operator's User"). An entry
	// naming anyone else, or no one yet, fails boot; the error names the
	// entry's position, never its secret.
	if err := api.ResolveAPITokens(ctx); err != nil {
		return err
	}
	if len(apiTokens) > 0 {
		logger.Info("static API tokens resolved to operator users", "tokens", len(apiTokens))
	}

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
