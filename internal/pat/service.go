package pat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/id"
	"github.com/stump-wtf/cairn/internal/user"
)

// maxNameLength bounds the caller-supplied label so a pathological value
// cannot bloat the settings listing.
const maxNameLength = 128

// Service is the personal-access-token core over Postgres. It shares the
// same pool the artifact store and other core services use (ADR-0012: one
// binary, one database) and every query is parameterized (SPEC-0007 REQ
// "Database Operation Standards").
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewService builds the PAT core over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// Create mints a new personal access token owned by userID. scopes MUST
// already be validated (ParseScope) by the caller; an empty/nil scopes slice
// is rejected — a token that can do nothing is a caller mistake, not a valid
// grant. The plaintext secret is returned exactly once; only its SHA-256
// digest is persisted (SPEC-0007 REQ "Database Operation Standards").
func (s *Service) Create(ctx context.Context, userID, name string, scopes []string, isAgent bool) (secret string, tok *Token, err error) {
	if !user.ValidID(userID) {
		return "", nil, errs.Validationf("tokens: owner is required")
	}
	if name == "" {
		return "", nil, errs.Validationf("tokens: name is required")
	}
	if len(name) > maxNameLength {
		return "", nil, errs.Validationf("tokens: name must be %d characters or fewer", maxNameLength)
	}
	if len(scopes) == 0 {
		return "", nil, errs.Validationf("tokens: at least one scope is required")
	}
	tokenID, err := id.NewLength(20)
	if err != nil {
		return "", nil, fmt.Errorf("pat: mint token id: %w", err)
	}
	tokenID = "pat-" + tokenID
	secret, err = newSecret()
	if err != nil {
		return "", nil, err
	}
	now := s.now().UTC()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO personal_access_tokens (id, user_id, name, token_hash, scope, is_agent, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tokenID, userID, name, hashSecret(secret), JoinScope(scopes), isAgent, now,
	); err != nil {
		return "", nil, fmt.Errorf("pat: insert token: %w", err)
	}
	return secret, &Token{
		ID:        tokenID,
		UserID:    userID,
		Name:      name,
		Scopes:    scopes,
		IsAgent:   isAgent,
		CreatedAt: now,
	}, nil
}

// List returns userID's tokens (newest first), metadata only — the plaintext
// secret is never stored so it is never returned here (SPEC-0002 acceptance:
// "list, metadata only — never the secret").
func (s *Service) List(ctx context.Context, userID string) ([]*Token, error) {
	if userID == "" {
		return nil, errs.Validationf("tokens: owner is required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenColumns+`
		FROM personal_access_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.user_id = $1
		ORDER BY t.created_at DESC`,
		user.IDParam(userID),
	)
	if err != nil {
		return nil, fmt.Errorf("pat: list tokens: %w", err)
	}
	defer rows.Close()

	var out []*Token
	for rows.Next() {
		t, scope, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("pat: scan token: %w", err)
		}
		t.Scopes, err = ParseScope(scope)
		if err != nil {
			return nil, fmt.Errorf("pat: stored scope invalid for token %s: %w", t.ID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pat: list tokens: %w", err)
	}
	return out, nil
}

// tokenColumns is the projection scanToken reads, over a token row t joined
// to its owning user u (the owner renders from the user row).
var tokenColumns = `t.id, t.user_id::text, ` + user.ActorSQL("u") + `, t.name, t.scope, t.is_agent,
	t.created_at, t.last_used_at, t.revoked_at`

// rowScanner is the subset of pgx.Rows / pgx.Row Scan needs.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanToken scans one token row into everything but the parsed Scopes field
// (the caller parses the raw scope string, since Create's single-row
// insert path never needs a round trip through ParseScope).
func scanToken(row rowScanner) (*Token, string, error) {
	var (
		t     Token
		scope string
	)
	if err := row.Scan(&t.ID, &t.UserID, &t.Actor, &t.Name, &scope, &t.IsAgent, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
		return nil, "", err
	}
	return &t, scope, nil
}

// Revoke marks userID's token id revoked. It is owner-scoped and uniform:
// an id that does not exist, belongs to a different owner, or is already
// revoked all resolve to errs.ErrNotFound, so the endpoint leaks nothing
// about another owner's tokens (mirrors the artifact store's owner-scoped
// delete, internal/store/delete.go).
func (s *Service) Revoke(ctx context.Context, userID, tokenID string) error {
	if userID == "" || tokenID == "" {
		return errs.Validationf("tokens: owner and id are required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE personal_access_tokens
		SET revoked_at = $1
		WHERE id = $2 AND user_id = $3 AND revoked_at IS NULL`,
		s.now().UTC(), tokenID, user.IDParam(userID),
	)
	if err != nil {
		return fmt.Errorf("pat: revoke token %s: %w", tokenID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token %s: %w", tokenID, errs.ErrNotFound)
	}
	return nil
}

// Authenticate resolves a presented bearer secret to its live (unrevoked)
// token, touching last_used_at atomically in the same statement so every
// successful authentication updates it with no extra round trip (issue #74
// acceptance: "Update last_used_at on use"). Any failure — unknown or
// revoked secret — is errs.ErrUnauthorized; the lookup is a single hashed,
// parameterized query, so cost does not vary with which token matched
// (SPEC-0007 REQ "Database Operation Standards").
func (s *Service) Authenticate(ctx context.Context, secret string) (*Token, error) {
	if secret == "" {
		return nil, errs.ErrUnauthorized
	}
	now := s.now().UTC()
	row := s.pool.QueryRow(ctx, `
		WITH t AS (
			UPDATE personal_access_tokens
			SET last_used_at = $1
			WHERE token_hash = $2 AND revoked_at IS NULL
			RETURNING *)
		SELECT `+tokenColumns+` FROM t JOIN users u ON u.id = t.user_id`,
		now, hashSecret(secret),
	)
	t, scope, err := scanToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("pat: authenticate: %w", err)
	}
	t.Scopes, err = ParseScope(scope)
	if err != nil {
		return nil, fmt.Errorf("pat: stored scope invalid for token %s: %w", t.ID, err)
	}
	return t, nil
}
