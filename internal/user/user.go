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
// Every owner and actor reference in the schema is a user id (SPEC-0023 REQ
// "Owner Model"). A user renders on the wire (actor_id, provenance) as its
// primary email, else its actor key, else "user:<id>" (ActorSQL). The actor
// key is the string a user without a primary email is known by: the legacy
// owner string the ownership migration created it from, the name a
// string-keyed credential names it by (ResolveActor), or its OIDC subject.
//
// Governing: ADR-0029, SPEC-0023 REQ "Users and Identities", REQ "Owner
// Model", REQ "Migration to Explicit Ownership".
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
	// ActorKey is the string-keyed name of a user without a primary email;
	// empty when it has none.
	ActorKey    string
	SuspendedAt *time.Time
	CreatedAt   time.Time
	// Actor is what this user renders as on the wire (ActorSQL).
	Actor string
}

// ActorSQL is the SQL expression a users row, aliased alias, renders as on the
// wire: its primary email, else its actor key, else "user:<id>". On a LEFT
// JOIN with no row it is NULL; wrap it in COALESCE where that can happen.
func ActorSQL(alias string) string {
	return fmt.Sprintf("COALESCE(%[1]s.primary_email, %[1]s.actor_key, 'user:' || %[1]s.id::text)", alias)
}

// IDParam passes a user id as a query parameter: the id itself when it is a
// well-formed uuid, else NULL, so a malformed or empty id matches no row
// instead of failing the query (fail closed).
func IDParam(id string) any {
	if ValidID(id) {
		return id
	}
	return nil
}

// ValidID reports whether id is a canonical, lower-case uuid as Postgres
// renders one.
func ValidID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
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

// linkOrCreate settles the user a first-time identity belongs to:
//
//  1. the user whose verified primary email equals the identity's verified
//     email;
//  2. else a legacy user (SPEC-0023 REQ "Migration to Explicit Ownership")
//     whose actor key equals that verified email, ignoring case, which this
//     sign-in claims: the email becomes its verified primary email;
//  3. else a new user.
//
// An unverified email is never compared and never stored as a primary email,
// so it cannot claim anyone (SPEC-0023 "Unverified email cannot claim a
// user"). A new user without a verified email keeps its subject as its actor
// key when the subject may name it (subjectIsActorKey) and no other user
// holds that key.
func (s *Store) linkOrCreate(ctx context.Context, tx pgx.Tx, id Identity) (string, error) {
	var primary, actorKey any
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
		// The claim. An exact-case legacy string wins over a case variant of
		// it; among variants the oldest does.
		err = tx.QueryRow(ctx, `
			UPDATE users SET primary_email = $1, email_verified = true
			 WHERE id = (SELECT id FROM users
			              WHERE primary_email IS NULL AND NOT email_verified
			                AND lower(actor_key) = $1
			              ORDER BY actor_key = $1 DESC, created_at, id
			              LIMIT 1
			              FOR UPDATE)
			RETURNING id::text`,
			id.Email,
		).Scan(&userID)
		switch {
		case err == nil:
			return userID, nil
		case isUniqueViolation(err):
			return "", errRetry
		case !errors.Is(err, pgx.ErrNoRows):
			return "", fmt.Errorf("user: claim legacy owner: %w", err)
		}
	} else if subjectIsActorKey(id.Subject) {
		actorKey = id.Subject
	}
	var userID string
	err := tx.QueryRow(ctx, `
		INSERT INTO users (primary_email, email_verified, display_handle, actor_key)
		VALUES ($1, $2, $3,
		        CASE WHEN NOT EXISTS (SELECT 1 FROM users WHERE actor_key = $4::text)
		             THEN $4::text END)
		RETURNING id::text`,
		primary, primary != nil, displayHandleFor(id), actorKey,
	).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			// A concurrent first sign-in with the same verified email or the
			// same subject won: the retry links to the former and leaves the
			// key unset for the latter.
			return "", errRetry
		}
		return "", fmt.Errorf("user: create: %w", err)
	}
	return userID, nil
}

// subjectIsActorKey reports whether a provider subject may name its user on
// the wire. An email-shaped subject would render as an email nobody verified,
// and a "user:" one could imitate another user's id rendering.
func subjectIsActorKey(subject string) bool {
	return subject != "" && !strings.Contains(subject, "@") && !strings.HasPrefix(subject, "user:")
}

// ResolveActor returns the user a string-keyed credential names: the
// development login or the insecure development bearer. CAIRN_API_TOKENS
// entries never reach it; they name an existing operator through
// FindByIdentity or FindByVerifiedEmail and never create a user. key resolves to the user whose id it spells as "user:<id>", else the
// user whose actor key it is, else the user whose VERIFIED primary email it
// is, else a new unverified user with key as its actor key, which a later
// sign-in with that email verified claims. That is the order the ownership
// migration resolved legacy owner strings in, so a credential keeps the Bin it
// had before.
func (s *Store) ResolveActor(ctx context.Context, key string) (*User, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("user: actor key is required")
	}
	if rest, ok := strings.CutPrefix(key, "user:"); ok {
		return s.Get(ctx, rest)
	}
	for attempt := 0; ; attempt++ {
		u, err := scanUser(s.pool.QueryRow(ctx, selectUser+`
			 WHERE actor_key = $1 OR (primary_email = $1 AND email_verified)
			 ORDER BY actor_key = $1 DESC NULLS LAST
			 LIMIT 1`, key))
		if !errors.Is(err, ErrNotFound) {
			return u, err
		}
		u, err = scanUser(s.pool.QueryRow(ctx, `
			INSERT INTO users (actor_key, email_verified, display_handle)
			VALUES ($1, false, $2)
			RETURNING `+userColumns, key, HandleFromEmail(key)))
		if isUniqueViolation(err) && attempt == 0 {
			continue // a concurrent resolve created it; read theirs
		}
		return u, err
	}
}

// FindByIdentity returns the user a sign-in identity belongs to, or
// ErrNotFound when no one has signed in with it. Unlike Resolve it never
// creates anything: it is for configuration that must name a user who
// already exists (SPEC-0023 REQ "Static API Tokens Act as an Operator's
// User").
func (s *Store) FindByIdentity(ctx context.Context, issuer, subject string) (*User, error) {
	if issuer == "" || subject == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(ctx, selectUser+`
		 WHERE id = (SELECT user_id FROM user_identities WHERE issuer = $1 AND subject = $2)`,
		issuer, subject))
}

// FindByVerifiedEmail returns the user whose VERIFIED primary email is email
// (compared after NormalizeEmail), or ErrNotFound. An unverified email never
// names a user, and nothing is created.
func (s *Store) FindByVerifiedEmail(ctx context.Context, email string) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(ctx, selectUser+`
		 WHERE primary_email = $1 AND email_verified`, email))
}

// Get returns a user by id, or ErrNotFound (a malformed id included).
func (s *Store) Get(ctx context.Context, id string) (*User, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(ctx, selectUser+` WHERE id = $1`, id))
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

// userColumns is the users projection scanUser reads, after SELECT and
// RETURNING alike.
var userColumns = `id::text, COALESCE(primary_email, ''), email_verified, display_handle,
	COALESCE(actor_key, ''), suspended_at, created_at, ` + ActorSQL("users")

var selectUser = `SELECT ` + userColumns + ` FROM users`

// scanUser reads userColumns. A unique violation from an INSERT … RETURNING
// is returned unwrapped so callers can retry on it.
func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.PrimaryEmail, &u.EmailVerified, &u.DisplayHandle,
		&u.ActorKey, &u.SuspendedAt, &u.CreatedAt, &u.Actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, err
		}
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
