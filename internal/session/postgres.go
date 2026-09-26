package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stump-wtf/cairn/internal/user"
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
//
// The row records only the user: the actor a session acts as is rendered
// from that user on every Get (SPEC-0023 REQ "Migration to Explicit
// Ownership"), so actorID only fills the returned value.
func (s *PostgresStore) Create(ctx context.Context, issuer, subject, actorID, userID, operatorGroup string, ttl time.Duration) (*Session, error) {
	if actorID == "" {
		return nil, errors.New("session: actor id is required")
	}
	if !user.ValidID(userID) {
		return nil, errors.New("session: user id is required")
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
		UserID:    userID,
		Issuer:    issuer,
		Subject:   subject,
		CSRFToken: csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),

		OperatorGroup: operatorGroup,
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, issuer, subject, operator_group, csrf_token, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hashToken(token), userID, issuer, subject, operatorGroup, csrf, sess.CreatedAt, sess.ExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("session: insert: %w", err)
	}
	return sess, nil
}

// Get resolves a raw token to its live session. An expired row, and the row
// of a suspended user, are filtered in the query so they resolve as
// ErrNotFound, indistinguishable from an unknown or revoked token. Suspension
// deletes a user's sessions as well; the filter is what makes a session that
// raced that delete fail too (SPEC-0023 "Suspending a user offboards them").
func (s *PostgresStore) Get(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	sess := &Session{Token: token}
	err := s.pool.QueryRow(ctx, `
		SELECT `+user.ActorSQL("u")+`, s.user_id::text, s.issuer, s.subject, s.operator_group,
		       s.csrf_token, s.created_at, s.expires_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.suspended_at IS NULL`,
		hashToken(token),
	).Scan(&sess.ActorID, &sess.UserID, &sess.Issuer, &sess.Subject, &sess.OperatorGroup,
		&sess.CSRFToken, &sess.CreatedAt, &sess.ExpiresAt)
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
