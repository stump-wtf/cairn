// Package user owns Cairn's users and the sign-in identities attached to them
// (ADR-0029, SPEC-0023 REQ "Users and Identities"). Before it, the principal
// was whatever string an authenticator produced: an OIDC email claim taken
// unverified and un-normalised, a GitHub email, a dev-login string. An IdP
// that lets a user type their own email therefore let them become someone
// else (audit A5), and `Joe@` and `joe@` were two owners.
//
// A sign-in now resolves its identity by (issuer, subject). A NEW identity
// links to an existing user only when its provider asserts a verified email
// equal, after lower-casing, to that user's primary email; otherwise it gets a
// user of its own. An unverified email never links and never becomes a
// primary email.
//
// Governing: ADR-0029, SPEC-0023 REQ "Users and Identities".
package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DevIssuer is the issuer recorded for identities established by the
// development password login. It is not a URL, so no real provider's issuer
// can ever equal it.
const DevIssuer = "cairn:dev"

// Identity is what a provider asserted at sign-in. Email is only ever used to
// link when EmailVerified is true; Handle is a display-handle hint (a GitHub
// login, an OIDC preferred_username) used only when a user is created.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Handle        string
}

// User is one row of the users table. PrimaryEmail is empty for a user who
// has never signed in with a verified email.
type User struct {
	ID            string
	PrimaryEmail  string
	EmailVerified bool
	DisplayHandle string
	SuspendedAt   *time.Time
	CreatedAt     time.Time
}

// NormalizeEmail lower-cases and trims an email for storage and comparison.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Store is the Postgres-backed users store.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a users store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// errRetry signals a lost insert race inside Resolve; the transaction is
// retried once, when the winning row is visible.
var errRetry = errors.New("user: concurrent sign-in, retry")

// Resolve returns the user an identity belongs to, creating the identity (and
// a user, when it links to none) on its first sign-in. The identity's email
// and verification are refreshed on every sign-in, but a known identity keeps
// its user whatever email it now presents: linking happens once, at first
// sign-in, and only by verified email.
func (s *Store) Resolve(ctx context.Context, id Identity) (*User, error) {
	if id.Issuer == "" || id.Subject == "" {
		return nil, errors.New("user: identity needs an issuer and a subject")
	}
	id.Email = NormalizeEmail(id.Email)
	for attempt := 0; ; attempt++ {
		u, err := s.resolveOnce(ctx, id)
		if errors.Is(err, errRetry) && attempt == 0 {
			continue
		}
		return u, err
	}
}

func (s *Store) resolveOnce(ctx context.Context, id Identity) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("user: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var email any
	if id.Email != "" {
		email = id.Email
	}

	var userID string
	err = tx.QueryRow(ctx, `
		UPDATE user_identities
		   SET email = $3, email_verified = $4, last_seen_at = now()
		 WHERE issuer = $1 AND subject = $2
		RETURNING user_id::text`,
		id.Issuer, id.Subject, email, id.EmailVerified,
	).Scan(&userID)
	switch {
	case err == nil:
		// A known identity: its user was settled at first sign-in.
	case errors.Is(err, pgx.ErrNoRows):
		userID, err = s.linkOrCreate(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_identities (user_id, issuer, subject, email, email_verified)
			VALUES ($1, $2, $3, $4, $5)`,
			userID, id.Issuer, id.Subject, email, id.EmailVerified,
		); err != nil {
			if isUniqueViolation(err) {
				return nil, errRetry
			}
			return nil, fmt.Errorf("user: insert identity: %w", err)
		}
	default:
		return nil, fmt.Errorf("user: resolve identity: %w", err)
	}

	u, err := scanUser(tx.QueryRow(ctx, selectUser+` WHERE id = $1`, userID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("user: commit: %w", err)
	}
	return u, nil
}

// linkOrCreate settles the user a first-time identity belongs to: the user
// whose verified primary email equals the identity's verified email, or a new
// one. An unverified email is never compared and never stored as a primary
// email, so it cannot claim anyone (SPEC-0023 "Unverified email cannot claim a
// user").
func (s *Store) linkOrCreate(ctx context.Context, tx pgx.Tx, id Identity) (string, error) {
	var primary any
	if id.EmailVerified && id.Email != "" {
		primary = id.Email
		var userID string
		err := tx.QueryRow(ctx,
			`SELECT id::text FROM users WHERE primary_email = $1 AND email_verified`,
			id.Email,
		).Scan(&userID)
		if err == nil {
			return userID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("user: link by email: %w", err)
		}
	}
	var userID string
	err := tx.QueryRow(ctx, `
		INSERT INTO users (primary_email, email_verified, display_handle)
		VALUES ($1, $2, $3)
		RETURNING id::text`,
		primary, primary != nil, displayHandleFor(id),
	).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			// Either a concurrent first sign-in with the same verified email
			// won, or a legacy row holds the email unverified. Retry links to
			// the former; the latter is claimed by the ownership migration.
			return "", errRetry
		}
		return "", fmt.Errorf("user: create: %w", err)
	}
	return userID, nil
}

// Get returns a user by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, selectUser+` WHERE id::text = $1`, id))
}

// DisplayHandles maps each given email to the display handle of the user
// whose primary email it is. Emails with no user are absent from the result.
func (s *Store) DisplayHandles(ctx context.Context, emails []string) (map[string]string, error) {
	out := make(map[string]string, len(emails))
	norm := make([]string, 0, len(emails))
	for _, e := range emails {
		if e = NormalizeEmail(e); e != "" {
			norm = append(norm, e)
		}
	}
	if len(norm) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT primary_email, display_handle FROM users WHERE primary_email = ANY($1)`, norm)
	if err != nil {
		return nil, fmt.Errorf("user: display handles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email, handle string
		if err := rows.Scan(&email, &handle); err != nil {
			return nil, fmt.Errorf("user: scan handle: %w", err)
		}
		out[email] = handle
	}
	return out, rows.Err()
}

// ErrNotFound reports an unknown user id.
var ErrNotFound = errors.New("user: not found")

const selectUser = `
	SELECT id::text, COALESCE(primary_email, ''), email_verified, display_handle, suspended_at, created_at
	  FROM users`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.PrimaryEmail, &u.EmailVerified, &u.DisplayHandle, &u.SuspendedAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("user: scan: %w", err)
	}
	return &u, nil
}

// displayHandleFor picks a new user's display handle: the provider's handle
// hint, else the local part of the email, else a neutral placeholder. It is
// display text, not an identifier, so collisions are harmless.
func displayHandleFor(id Identity) string {
	if h := strings.TrimSpace(id.Handle); h != "" {
		return h
	}
	return HandleFromEmail(id.Email)
}

// HandleFromEmail derives a display handle from an email-shaped string: its
// local part. A string with no "@" is returned unchanged (it is not an email),
// and an empty one becomes "user".
func HandleFromEmail(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "user"
	}
	return s
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
