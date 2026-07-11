package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production session store: session records live in the
// same Postgres the artifact core uses, so the single binary carries their
// schema and a horizontally-scaled deployment shares one session table
// (ADR-0012). Only the token hash is persisted (see hashToken).
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresStore builds a Postgres-backed session Store over the shared pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, now: time.Now}
}

// Create mints and persists a session, returning it with its raw secrets. The
// row stores only the token's hash; the raw token is the cookie value the caller
// sets and never sees again from the store.
func (s *PostgresStore) Create(ctx context.Context, actorID string, ttl time.Duration) (*Session, error) {
	if actorID == "" {
		return nil, errors.New("session: actor id is required")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	token, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("session: mint token: %w", err)
	}
	csrf, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("session: mint csrf: %w", err)
	}
	now := s.now().UTC()
	sess := &Session{
		Token:     token,
		ActorID:   actorID,
		CSRFToken: csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, actor_id, csrf_token, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		hashToken(token), actorID, csrf, sess.CreatedAt, sess.ExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("session: insert: %w", err)
	}
	return sess, nil
}

// Get resolves a raw token to its live session. An expired row is filtered in
// the query so it resolves as ErrNotFound, indistinguishable from an unknown or
// revoked token.
func (s *PostgresStore) Get(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	sess := &Session{Token: token}
	err := s.pool.QueryRow(ctx, `
		SELECT actor_id, csrf_token, created_at, expires_at
		FROM sessions
		WHERE token_hash = $1 AND expires_at > now()`,
		hashToken(token),
	).Scan(&sess.ActorID, &sess.CSRFToken, &sess.CreatedAt, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: get: %w", err)
	}
	return sess, nil
}

// Delete revokes a session by its raw token. Deleting an absent token is a no-op.
func (s *PostgresStore) Delete(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}
