// Package pat implements Cairn's personal access tokens: human-minted bearer
// credentials for the /v1 surface, created and revoked from the web Settings
// UI (issue #74). A PAT is the same "one Authenticator seam" ADR-0004
// establishes for CAIRN_API_TOKENS and OAuth 2.1 — this package is a third
// implementation over the same seam, not a parallel auth mechanism: it plugs
// into internal/httpapi/auth.go's chainAuthenticator exactly like the static
// token authenticator and the OAuth bearer authenticator do (see
// httpapi.PATAuthenticator).
//
// Unlike an OAuth grant (short access token + rotating refresh, issued by an
// authorization-code flow) a PAT is a single long-lived opaque secret the
// owner mints directly: simpler, and the intended shape for pasting into an
// agent's config or a CLI profile (foundation for #21). Scopes are a
// caller-chosen subset of the three ADR-0004 consent scopes — this package
// reuses internal/oauth's scope constants and ParseScope/JoinScope rather
// than redefining them, since a PAT's grantable scopes are exactly that set
// and nothing else (no sharing:manage, no delete: SPEC-0007 REQ "Exactly
// Three Consent Scopes"). The is_agent flag is independent of scope: it
// marks whether this token was minted for an agent acting on the owner's
// behalf, which keeps human-only capabilities (delete; see
// httpapi.requireHuman) off regardless of which scopes were granted.
//
// Governing: ADR-0004 (token seam), SPEC-0007 (three-scope model, hashed-at-
// rest secrets, database operation standards).
package pat

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/joestump/cairn/internal/oauth"
)

// The three grantable scopes, re-exported from internal/oauth so callers
// (and this package's own tests) never need to import both packages just to
// name a scope.
const (
	ScopeArtifactsRead    = oauth.ScopeArtifactsRead
	ScopeArtifactsWrite   = oauth.ScopeArtifactsWrite
	ScopeAnnotationsWrite = oauth.ScopeAnnotationsWrite
)

// AllScopes, ParseScope, and JoinScope are the same three-scope vocabulary
// ADR-0004 defines for OAuth grants (internal/oauth), re-exported here so a
// PAT can never be minted with a scope outside that set — there is exactly
// one definition of "the three scopes" in the codebase.
func AllScopes() []string { return oauth.AllScopes() }

// ParseScope validates and canonicalizes a space-delimited scope string
// against the three-scope set (see internal/oauth.ParseScope).
func ParseScope(raw string) ([]string, error) { return oauth.ParseScope(raw) }

// JoinScope renders a canonical scope list as its space-delimited wire form.
func JoinScope(scopes []string) string { return oauth.JoinScope(scopes) }

// Token is a personal access token's metadata — everything except the
// plaintext secret, which exists only in memory at creation time (Service.Create
// return value) and is never persisted or re-derivable.
type Token struct {
	ID         string
	OwnerID    string
	Name       string
	Scopes     []string
	IsAgent    bool
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// secretPrefix marks a Cairn PAT so leaked strings are identifiable in
// scanners without weakening entropy — the same convention internal/oauth
// uses for its own secrets ("cairn_ac_", "cairn_at_", "cairn_rt_").
const secretPrefix = "cairn_pat_"

// IsSecret reports whether a bearer token looks like a Cairn personal access
// token (the "cairn_pat_" prefix). Callers use it to route a bearer to the PAT
// authenticator vs. the OAuth access-token verifier; the prefixes are disjoint
// ("cairn_pat_" vs "cairn_at_"), so this never misroutes a real token.
func IsSecret(token string) bool { return strings.HasPrefix(token, secretPrefix) }

// newSecret mints a 32-byte high-entropy URL-safe PAT secret.
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pat: mint secret: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret maps a raw secret to the lowercase-hex SHA-256 stored at rest,
// matching internal/oauth's convention so both packages' token_hash columns
// are directly comparable in shape (SPEC-0007 REQ "Database Operation
// Standards").
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
