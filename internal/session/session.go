// Package session is Cairn's minimal server-side web session store (SPEC-0001,
// ADR-0004). A browser authenticates once through the login surface and is
// issued an opaque, high-entropy session token carried in a secure httpOnly
// cookie; this package holds the server-side record that token resolves to — the
// actor identity and the session's bound CSRF token — so nothing sensitive lives
// in the cookie itself. It is the seam the future OAuth 2.1 authorization server
// (ADR-0004, #22) reuses for its own token-backed sessions: a different
// credential adapter, the same server-side session record.
//
// Tokens are stored hashed (SHA-256), never in the clear, so a database leak
// does not yield usable session cookies (the same treatment a password hash
// receives). Lookups hash the presented cookie and compare, and expired records
// resolve as absent.
//
// Governing: SPEC-0001 (Authentication & Authorization), ADR-0004 (auth seam).
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// ErrNotFound reports that a session token does not resolve to a live session —
// it is unknown, expired, or already revoked. The three are deliberately
// indistinguishable so a stale cookie leaks no signal (the same uniform-absence
// discipline as an unknown artifact id, ADR-0007).
var ErrNotFound = errors.New("session: not found")

// Session is one authenticated browser session. Token is the raw opaque value
// placed in the cookie and returned only at creation (the store persists only
// its hash); CSRFToken is the double-submit secret bound to this session and
// echoed in the readable cairn_csrf cookie so a same-origin script can prove the
// request came from Cairn's own origin. Issuer/Subject record the auth
// provenance (SPEC-0012): which provider established the session and the
// subject that provider asserted. Both are empty for the dev-password login.
type Session struct {
	Token     string
	ActorID   string
	Issuer    string
	Subject   string
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Store is the session persistence seam. The Postgres implementation backs the
// running server; the in-memory implementation backs unit tests. OAuth (#22)
// adds no new method — it mints sessions through Create like the dev login does.
type Store interface {
	// Create mints a new session living for ttl, returning it with its raw
	// Token and CSRFToken populated (both are shown to the caller exactly once,
	// then only their derivations are retained). issuer/subject record the
	// session's auth provenance (SPEC-0012): the provider that established it
	// and the subject that provider asserted — empty for the dev-password
	// login, the OIDC issuer and ID-token subject for Pocket ID, and the
	// GitHub origin and login for GitHub.
	Create(ctx context.Context, issuer, subject, actorID string, ttl time.Duration) (*Session, error)
	// Get resolves a raw session token to its live session, or ErrNotFound when
	// the token is unknown, expired, or revoked.
	Get(ctx context.Context, token string) (*Session, error)
	// Delete revokes the session addressed by its raw token. Revoking an absent
	// session is a no-op (logout is idempotent).
	Delete(ctx context.Context, token string) error
}

// NewToken mints a standalone high-entropy, URL-safe token. It backs the login
// form's pre-session CSRF token — the same entropy source as a session token, so
// the no-JS login double-submit is as unguessable as a live session's.
func NewToken() (string, error) { return newToken() }

// newToken returns 32 bytes of cryptographic randomness, URL-safe base64 encoded
// — the entropy that makes a session or CSRF token unguessable.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken maps a raw token to the lowercase-hex SHA-256 stored at rest. A DB
// disclosure therefore reveals only irreversible hashes, never live cookies.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
