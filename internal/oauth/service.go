package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/id"
	"github.com/stump-wtf/cairn/internal/user"
)

// Defaults for the token model (SPEC-0007 REQ "Token Model": ~1h access,
// longer-lived rotating refresh; authorization codes single-use and
// short-lived).
const (
	DefaultAccessTTL  = time.Hour
	DefaultRefreshTTL = 30 * 24 * time.Hour
	DefaultCodeTTL    = 5 * time.Minute
)

// Options tunes the Service; zero values take the defaults above.
type Options struct {
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	CodeTTL    time.Duration
}

// Service is the authorization-server core over Postgres. Every multi-step
// state change (code redemption minting a grant + token family; refresh
// rotation; revocation cascade) runs in a single transaction so a grant is
// never half-written (SPEC-0007 REQ "Database Operation Standards"), and every
// query is parameterized.
type Service struct {
	pool       *pgxpool.Pool
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration
	codeTTL    time.Duration
	now        func() time.Time
}

// NewService builds the authorization-server core. audience is the RFC 8707
// resource identifier access tokens are bound to — Cairn's own public origin —
// so a token minted here is rejected by any other audience (SPEC-0007 scenario
// "Access token bound to audience").
func NewService(pool *pgxpool.Pool, audience string, opts Options) *Service {
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = DefaultAccessTTL
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = DefaultRefreshTTL
	}
	if opts.CodeTTL <= 0 {
		opts.CodeTTL = DefaultCodeTTL
	}
	return &Service{
		pool:       pool,
		audience:   audience,
		accessTTL:  opts.AccessTTL,
		refreshTTL: opts.RefreshTTL,
		codeTTL:    opts.CodeTTL,
		now:        time.Now,
	}
}

// Audience returns the resource identifier this server binds access tokens to.
func (s *Service) Audience() string { return s.audience }

// RegisterClient persists a dynamically-registered public client (RFC 7591).
// Every redirect URI must pass the registration rules; a violation is
// ErrInvalidRedirect and nothing is stored (SPEC-0007 scenario "Registration
// with an invalid redirect URI").
func (s *Service) RegisterClient(ctx context.Context, name string, redirectURIs []string) (*Client, error) {
	if len(redirectURIs) == 0 {
		return nil, fmt.Errorf("at least one redirect_uri is required: %w", ErrInvalidRedirect)
	}
	if len(redirectURIs) > 10 {
		return nil, fmt.Errorf("too many redirect_uris: %w", ErrInvalidRequest)
	}
	for _, u := range redirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			return nil, err
		}
	}
	if len(name) > 256 {
		return nil, fmt.Errorf("client_name too long: %w", ErrInvalidRequest)
	}
	cid, err := id.NewLength(24)
	if err != nil {
		return nil, fmt.Errorf("oauth: mint client id: %w", err)
	}
	c := &Client{ID: "cairn-" + cid, Name: name, RedirectURIs: redirectURIs, CreatedAt: s.now().UTC()}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_clients (client_id, client_name, redirect_uris, created_at)
		VALUES ($1, $2, $3, $4)`,
		c.ID, c.Name, c.RedirectURIs, c.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("oauth: insert client: %w", err)
	}
	return c, nil
}

// GetClient resolves a client id, or ErrInvalidClient when unknown.
func (s *Service) GetClient(ctx context.Context, clientID string) (*Client, error) {
	if clientID == "" {
		return nil, fmt.Errorf("client_id is required: %w", ErrInvalidClient)
	}
	c := &Client{ID: clientID}
	err := s.pool.QueryRow(ctx, `
		SELECT client_name, redirect_uris, created_at
		FROM oauth_clients WHERE client_id = $1`,
		clientID,
	).Scan(&c.Name, &c.RedirectURIs, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("unknown client %q: %w", clientID, ErrInvalidClient)
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: get client: %w", err)
	}
	return c, nil
}

// CreateAuthCode mints a single-use authorization code after consent approval,
// bound to the client, the exact redirect URI presented, the approved scope
// subset, and the PKCE S256 challenge (SPEC-0007: codes "single-use,
// short-lived, and bound to the client, redirect URI, and PKCE challenge").
// The raw code is returned exactly once; only its hash is stored.
func (s *Service) CreateAuthCode(ctx context.Context, clientID, userID, redirectURI string, scopes []string, challenge string) (string, error) {
	if clientID == "" || !user.ValidID(userID) || redirectURI == "" || len(scopes) == 0 {
		return "", fmt.Errorf("code requires client, actor, redirect uri, and scopes: %w", ErrInvalidRequest)
	}
	if !ValidChallenge(challenge) {
		return "", fmt.Errorf("a valid S256 code_challenge is required: %w", ErrInvalidRequest)
	}
	code, err := newSecret("cairn_ac_")
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO oauth_auth_codes
			(code_hash, client_id, user_id, redirect_uri, scope, code_challenge, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hashSecret(code), clientID, userID, redirectURI, JoinScope(scopes), challenge, now, now.Add(s.codeTTL),
	); err != nil {
		return "", fmt.Errorf("oauth: insert auth code: %w", err)
	}
	return code, nil
}

// RedeemCode exchanges an authorization code + PKCE verifier for a fresh grant
// and its token family, atomically: the code is marked spent, the grant row
// created, and both tokens inserted in one transaction. Failure modes are all
// ErrInvalidGrant (unknown/expired code, client or redirect mismatch, bad
// verifier) so the token endpoint leaks nothing about which check failed. A
// replayed (already-redeemed) code additionally revokes the grant it minted
// (SPEC-0007 scenario "Replayed authorization code").
func (s *Service) RedeemCode(ctx context.Context, code, clientID, redirectURI, verifier string) (*TokenSet, error) {
	if code == "" || clientID == "" {
		return nil, fmt.Errorf("code and client_id are required: %w", ErrInvalidGrant)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth: begin redeem: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		rowClientID, rowUser, rowRedirect, rowScope, rowChallenge string
		expiresAt                                                 time.Time
		redeemedAt, suspendedAt                                   *time.Time
		priorGrantID                                              *string
	)
	err = tx.QueryRow(ctx, `
		SELECT c.client_id, c.user_id::text, c.redirect_uri, c.scope, c.code_challenge,
		       c.expires_at, c.redeemed_at, c.grant_id, u.suspended_at
		FROM oauth_auth_codes c JOIN users u ON u.id = c.user_id
		WHERE c.code_hash = $1 FOR UPDATE OF c`,
		hashSecret(code),
	).Scan(&rowClientID, &rowUser, &rowRedirect, &rowScope, &rowChallenge, &expiresAt, &redeemedAt, &priorGrantID, &suspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("unknown authorization code: %w", ErrInvalidGrant)
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: load auth code: %w", err)
	}
	now := s.now().UTC()
	if redeemedAt != nil {
		// Replay: the code was already spent. Revoke the token family it minted
		// so a stolen code cannot coexist with the legitimate exchange.
		if priorGrantID != nil {
			if err := revokeGrantTx(ctx, tx, *priorGrantID, now); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("oauth: commit replay revocation: %w", err)
		}
		return nil, fmt.Errorf("authorization code already redeemed: %w", ErrInvalidGrant)
	}
	if now.After(expiresAt) {
		return nil, fmt.Errorf("authorization code expired: %w", ErrInvalidGrant)
	}
	// A failed exchange attempt BURNS the code (single-use means one attempt,
	// not one success): a stolen code cannot be used to brute-force the
	// verifier, and the legitimate holder's later exchange fails loudly.
	burn := func(reason string) (*TokenSet, error) {
		if _, err := tx.Exec(ctx, `
			UPDATE oauth_auth_codes SET redeemed_at = $1 WHERE code_hash = $2`,
			now, hashSecret(code),
		); err == nil {
			_ = tx.Commit(ctx)
		}
		return nil, fmt.Errorf("%s: %w", reason, ErrInvalidGrant)
	}
	if suspendedAt != nil {
		// Suspension deletes unredeemed codes; this burns one that raced it
		// (SPEC-0023 "Suspending a user offboards them").
		return burn("user suspended")
	}
	if rowClientID != clientID {
		return burn("code was not issued to this client")
	}
	if rowRedirect != redirectURI {
		return burn("redirect_uri does not match the code's binding")
	}
	if !VerifyPKCE(verifier, rowChallenge) {
		// PKCE is mandatory for every client: no verifier, wrong verifier, and
		// malformed verifier are indistinguishable failures issuing no tokens
		// (SPEC-0007 scenario "Code exchange without a verifier").
		return burn("pkce verification failed")
	}

	gid, err := id.NewLength(16)
	if err != nil {
		return nil, fmt.Errorf("oauth: mint grant id: %w", err)
	}
	grantID := "grant-" + gid
	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_grants (grant_id, client_id, user_id, scope, created_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $5)`,
		grantID, clientID, rowUser, rowScope, now,
	); err != nil {
		return nil, fmt.Errorf("oauth: insert grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_auth_codes SET redeemed_at = $1, grant_id = $2 WHERE code_hash = $3`,
		now, grantID, hashSecret(code),
	); err != nil {
		return nil, fmt.Errorf("oauth: mark code redeemed: %w", err)
	}
	set, err := s.issueTokensTx(ctx, tx, grantID, rowScope, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("oauth: commit redeem: %w", err)
	}
	return set, nil
}

// Refresh rotates a refresh token: the presented token is invalidated and a
// new access + refresh pair issued, atomically, so a crash can never leave two
// valid or zero valid refresh tokens (SPEC-0007 scenario "Atomic refresh
// rotation"). Presenting a rotated-out or revoked refresh token is reuse —
// the whole token family (the grant) is revoked (scenario "Refresh rotation
// and reuse detection").
func (s *Service) Refresh(ctx context.Context, refreshToken, clientID string) (*TokenSet, error) {
	if refreshToken == "" || clientID == "" {
		return nil, fmt.Errorf("refresh_token and client_id are required: %w", ErrInvalidGrant)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("oauth: begin refresh: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var (
		grantID, kind, grantClient, scope               string
		tokenExpires                                    time.Time
		rotatedAt, revokedAt, grantRevoked, suspendedAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT t.grant_id, t.kind, t.expires_at, t.rotated_at, t.revoked_at,
		       g.client_id, g.scope, g.revoked_at, u.suspended_at
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.grant_id = t.grant_id
		JOIN users u ON u.id = g.user_id
		WHERE t.token_hash = $1 FOR UPDATE OF t, g`,
		hashSecret(refreshToken),
	).Scan(&grantID, &kind, &tokenExpires, &rotatedAt, &revokedAt, &grantClient, &scope, &grantRevoked, &suspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("unknown refresh token: %w", ErrInvalidGrant)
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: load refresh token: %w", err)
	}
	now := s.now().UTC()
	if kind != "refresh" {
		return nil, fmt.Errorf("token is not a refresh token: %w", ErrInvalidGrant)
	}
	if grantClient != clientID {
		return nil, fmt.Errorf("refresh token was not issued to this client: %w", ErrInvalidGrant)
	}
	if rotatedAt != nil || revokedAt != nil {
		// Reuse of a rotated-out (or revoked) refresh token: assume theft and
		// revoke the entire family, then surface invalid_grant.
		if err := revokeGrantTx(ctx, tx, grantID, now); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("oauth: commit reuse revocation: %w", err)
		}
		return nil, fmt.Errorf("refresh token reuse detected, family revoked: %w", ErrInvalidGrant)
	}
	if grantRevoked != nil {
		return nil, fmt.Errorf("grant is revoked: %w", ErrInvalidGrant)
	}
	if suspendedAt != nil {
		return nil, fmt.Errorf("user suspended: %w", ErrInvalidGrant)
	}
	if now.After(tokenExpires) {
		return nil, fmt.Errorf("refresh token expired: %w", ErrInvalidGrant)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_tokens SET rotated_at = $1, revoked_at = $1 WHERE token_hash = $2`,
		now, hashSecret(refreshToken),
	); err != nil {
		return nil, fmt.Errorf("oauth: rotate out refresh token: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_grants SET last_used_at = $1 WHERE grant_id = $2`,
		now, grantID,
	); err != nil {
		return nil, fmt.Errorf("oauth: touch grant: %w", err)
	}
	set, err := s.issueTokensTx(ctx, tx, grantID, scope, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("oauth: commit refresh: %w", err)
	}
	return set, nil
}

// issueTokensTx mints and persists one access + refresh pair for a grant
// inside the caller's transaction. Access tokens are audience-bound to the
// Cairn resource server (RFC 8707).
func (s *Service) issueTokensTx(ctx context.Context, tx pgx.Tx, grantID, scope string, now time.Time) (*TokenSet, error) {
	access, err := newSecret("cairn_at_")
	if err != nil {
		return nil, err
	}
	refresh, err := newSecret("cairn_rt_")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_tokens (token_hash, grant_id, kind, audience, created_at, expires_at)
		VALUES ($1, $2, 'access', $3, $4, $5), ($6, $2, 'refresh', $3, $4, $7)`,
		hashSecret(access), grantID, s.audience, now, now.Add(s.accessTTL),
		hashSecret(refresh), now.Add(s.refreshTTL),
	); err != nil {
		return nil, fmt.Errorf("oauth: insert token family: %w", err)
	}
	return &TokenSet{
		GrantID:         grantID,
		AccessToken:     access,
		RefreshToken:    refresh,
		AccessExpiresIn: int64(s.accessTTL / time.Second),
		Scope:           scope,
	}, nil
}

// revokeGrantTx revokes a grant and every token in its family inside the
// caller's transaction (the RFC 7009 / reuse-detection cascade).
func revokeGrantTx(ctx context.Context, tx pgx.Tx, grantID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_grants SET revoked_at = COALESCE(revoked_at, $1) WHERE grant_id = $2`,
		now, grantID,
	); err != nil {
		return fmt.Errorf("oauth: revoke grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE oauth_tokens SET revoked_at = COALESCE(revoked_at, $1) WHERE grant_id = $2`,
		now, grantID,
	); err != nil {
		return fmt.Errorf("oauth: revoke grant tokens: %w", err)
	}
	return nil
}

// Revoke implements RFC 7009 semantics over a presented token (access or
// refresh): when the token resolves to a grant owned by the presenting
// client, the entire grant family is revoked — access and refresh together,
// without touching the human's sibling grants (SPEC-0007 scenario "Revoke one
// connection"). An unknown token is NOT an error (RFC 7009 §2.2: the client
// asked for an already-invalid credential to be invalid). A token owned by a
// different client is treated as unknown, leaking nothing.
func (s *Service) Revoke(ctx context.Context, token, clientID string) error {
	if token == "" {
		return fmt.Errorf("token is required: %w", ErrInvalidRequest)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("oauth: begin revoke: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var (
		grantID, grantClient string
	)
	err = tx.QueryRow(ctx, `
		SELECT t.grant_id, g.client_id
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.grant_id = t.grant_id
		WHERE t.token_hash = $1 FOR UPDATE OF t, g`,
		hashSecret(token),
	).Scan(&grantID, &grantClient)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // unknown token: success per RFC 7009
	}
	if err != nil {
		return fmt.Errorf("oauth: load token for revocation: %w", err)
	}
	if clientID == "" || grantClient != clientID {
		return nil // not this client's token: indistinguishable from unknown
	}
	if err := revokeGrantTx(ctx, tx, grantID, s.now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("oauth: commit revoke: %w", err)
	}
	return nil
}

// RevokeGrant revokes a grant (and its whole token family) by id — the
// settings-page path ("revoke anytime in settings").
func (s *Service) RevokeGrant(ctx context.Context, grantID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("oauth: begin revoke grant: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := revokeGrantTx(ctx, tx, grantID, s.now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("oauth: commit revoke grant: %w", err)
	}
	return nil
}

// AuthenticateAccess resolves a presented access token to its identity: the
// human subject, the client, and the granted scopes. It fails with
// ErrInvalidGrant for anything not a live, audience-valid access token —
// unknown, expired, revoked, wrong kind, or bound to a different audience
// (SPEC-0007 scenario "Access token bound to audience", scenario "Revoked
// token used"). Lookup is by hash, parameterized, constant-cost.
func (s *Service) AuthenticateAccess(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, fmt.Errorf("no token presented: %w", ErrInvalidGrant)
	}
	var (
		kind, audience, grantID, clientID, userID, actor, scope string
		expiresAt                                               time.Time
		revokedAt, grantRevoked, suspendedAt                    *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.kind, t.audience, t.expires_at, t.revoked_at,
		       g.grant_id, g.client_id, g.user_id::text, `+user.ActorSQL("u")+`, g.scope, g.revoked_at,
		       u.suspended_at
		FROM oauth_tokens t
		JOIN oauth_grants g ON g.grant_id = t.grant_id
		JOIN users u ON u.id = g.user_id
		WHERE t.token_hash = $1`,
		hashSecret(token),
	).Scan(&kind, &audience, &expiresAt, &revokedAt, &grantID, &clientID, &userID, &actor, &scope, &grantRevoked, &suspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("unknown access token: %w", ErrInvalidGrant)
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: load access token: %w", err)
	}
	now := s.now().UTC()
	switch {
	case kind != "access":
		return nil, fmt.Errorf("token is not an access token: %w", ErrInvalidGrant)
	case audience != s.audience:
		return nil, fmt.Errorf("token audience mismatch: %w", ErrInvalidGrant)
	case revokedAt != nil || grantRevoked != nil:
		return nil, fmt.Errorf("token revoked: %w", ErrInvalidGrant)
	case suspendedAt != nil:
		// Suspension revokes the user's grants too; this refuses a token
		// that raced the revocation (SPEC-0023 "Suspending a user offboards
		// them").
		return nil, fmt.Errorf("user suspended: %w", ErrInvalidGrant)
	case now.After(expiresAt):
		return nil, fmt.Errorf("token expired: %w", ErrInvalidGrant)
	}
	scopes, err := ParseScope(scope)
	if err != nil {
		return nil, fmt.Errorf("oauth: stored scope invalid: %w", err)
	}
	return &Identity{UserID: userID, ActorID: actor, ClientID: clientID, GrantID: grantID, Scopes: scopes, ExpiresAt: expiresAt}, nil
}
