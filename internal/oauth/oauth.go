// Package oauth is Cairn's in-process OAuth 2.1 authorization server core
// (SPEC-0007, ADR-0004). It implements the authorization-code grant with PKCE
// mandatory (S256 only), RFC 7591 dynamic client registration, RFC 8707
// audience-bound access tokens with rotating refresh tokens, and RFC 7009
// revocation — persisting clients, codes, grants, and token families in the
// same Postgres the artifact core uses (ADR-0012: one binary, one database).
//
// The package is transport-free: the httpapi adapter renders the discovery
// documents, the consent screen, and the RFC-shaped endpoint responses on top
// of this service, exactly as the web/CLI adapters sit over the artifact core
// (ADR-0003). Secrets (codes, access tokens, refresh tokens) are returned to
// the caller exactly once and persisted only as SHA-256 digests.
//
// Governing: ADR-0004 (MCP OAuth 2.1 with scoped consent),
// SPEC-0007 REQ "OAuth 2.1 Authorization-Code + PKCE",
// SPEC-0007 REQ "Exactly Three Consent Scopes",
// SPEC-0007 REQ "Token Model — Short Audience-Bound Access + Rotating Refresh".
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// The exactly-three consent scopes of ADR-0004 / SPEC-0007. The authorization
// server recognizes these and nothing else — no sharing:manage, no
// artifacts:delete — so an agent grant can never exceed the consent screen
// (SPEC-0007 REQ "Exactly Three Consent Scopes").
const (
	ScopeArtifactsRead    = "artifacts:read"
	ScopeArtifactsWrite   = "artifacts:write"
	ScopeAnnotationsWrite = "annotations:write"
)

// AllScopes is the canonical ordered scope list — the three consent lines.
func AllScopes() []string {
	return []string{ScopeArtifactsRead, ScopeArtifactsWrite, ScopeAnnotationsWrite}
}

// ConsentLine maps a scope to its exact consent-screen line (SPEC-0007 REQ
// "Consent Screen Content"). Unknown scopes have no line and return "".
func ConsentLine(scope string) string {
	switch scope {
	case ScopeArtifactsRead:
		return "Read artifacts you can access"
	case ScopeArtifactsWrite:
		return "Create & push new artifacts"
	case ScopeAnnotationsWrite:
		return "Comment & react on your behalf"
	}
	return ""
}

// Sentinel protocol errors the transport adapter maps onto RFC 6749 error
// codes. Wrapping with %w keeps them discoverable via errors.Is (SPEC-0007 REQ
// "Error Handling Standards": distinguishable typed errors at layer
// boundaries).
var (
	ErrInvalidRequest  = errors.New("oauth: invalid_request")
	ErrInvalidClient   = errors.New("oauth: invalid_client")
	ErrInvalidGrant    = errors.New("oauth: invalid_grant")
	ErrInvalidScope    = errors.New("oauth: invalid_scope")
	ErrInvalidTarget   = errors.New("oauth: invalid_target")
	ErrInvalidRedirect = errors.New("oauth: invalid_redirect_uri")
)

// ParseScope validates and canonicalizes a space-delimited scope string
// against the exactly-three scope set. An empty input defaults to all three
// (the client asks for the full consent screen). Any token outside the three
// is ErrInvalidScope — the server never issues an unknown or elevated scope
// (SPEC-0007 scenario "Unknown or elevated scope requested"). The result is
// deduplicated and in canonical order.
func ParseScope(raw string) ([]string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return AllScopes(), nil
	}
	want := map[string]bool{}
	for _, f := range fields {
		if ConsentLine(f) == "" {
			return nil, fmt.Errorf("scope %q is not one of the three consent scopes: %w", f, ErrInvalidScope)
		}
		want[f] = true
	}
	var out []string
	for _, s := range AllScopes() {
		if want[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// JoinScope renders a canonical scope list as the space-delimited wire form.
func JoinScope(scopes []string) string { return strings.Join(scopes, " ") }

// --- PKCE (RFC 7636, S256 only) ---------------------------------------------

// ValidVerifier reports whether v is a syntactically legal PKCE code_verifier:
// 43–128 characters of the unreserved set ALPHA / DIGIT / "-" / "." / "_" /
// "~" (RFC 7636 §4.1).
func ValidVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// ValidChallenge reports whether c is shaped like an S256 code_challenge: the
// unpadded base64url form of a SHA-256 digest is exactly 43 characters.
func ValidChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil
}

// S256Challenge derives the S256 code_challenge for a verifier — used by tests
// and the CLI client, and the single definition the server verifies against.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyPKCE reports whether verifier satisfies the stored S256 challenge
// under a constant-time compare. A malformed verifier fails closed — the token
// endpoint issues nothing without a valid verifier (SPEC-0007 scenario "Code
// exchange without a verifier").
func VerifyPKCE(verifier, challenge string) bool {
	if !ValidVerifier(verifier) || challenge == "" {
		return false
	}
	derived := S256Challenge(verifier)
	return subtle.ConstantTimeCompare([]byte(derived), []byte(challenge)) == 1
}

// --- Redirect URI validation (OAuth 2.1 / RFC 8252) --------------------------

// ValidateRedirectURI enforces the registration-time redirect rules: the URI
// must be absolute with no fragment, and its scheme must be https — or http
// only for a loopback host (native/CLI clients, RFC 8252 §7.3). Everything
// else (custom schemes, plain-http remote hosts, relative paths) is rejected,
// so a hostile registration cannot point the authorization response at an
// attacker-controlled or server-side-reachable target (SPEC-0007 REQ
// "Redirect & SSRF Validation").
//
// The host itself must be a syntactically clean hostname or IP literal
// (ValidHostSyntax). url.Parse is permissive about what it accepts as a
// Host — e.g. "https://a;b/c" parses cleanly with Host `a;b` — so without
// this check a registration could smuggle a delimiter character through
// registration and into the consent screen's form-action CSP directive
// (consentCSP), producing a malformed rather than weakened header (#64).
func ValidateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect uri %q: %w", raw, ErrInvalidRedirect)
	}
	if !u.IsAbs() || u.Host == "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("redirect uri %q must be absolute, host-bearing, fragment- and userinfo-free: %w", raw, ErrInvalidRedirect)
	}
	if !ValidHostSyntax(u.Hostname()) {
		return fmt.Errorf("redirect uri %q: host %q contains characters outside the hostname charset: %w", raw, u.Hostname(), ErrInvalidRedirect)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("redirect uri %q: http is allowed only for loopback hosts: %w", raw, ErrInvalidRedirect)
	default:
		return fmt.Errorf("redirect uri %q: scheme %q is not allowed: %w", raw, u.Scheme, ErrInvalidRedirect)
	}
}

// isLoopbackHost reports whether host names the local loopback interface.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidHostSyntax reports whether host (as returned by url.URL.Hostname,
// i.e. with any IPv6 brackets already stripped) is a syntactically legal
// hostname or IP literal: an IPv4/IPv6 address, or the RFC 1123 "preferred
// name syntax" charset — ALPHA / DIGIT / "-" / "." only.
//
// url.Parse does not enforce this: it happily accepts delimiter characters
// like ";" into Host as long as they don't collide with its own port/query
// splitting, e.g. "https://a;b/c" parses with Host `a;b`. Both
// ValidateRedirectURI (registration time) and consentCSP (the value gets
// interpolated into a CSP directive) call this so such a host is rejected
// or falls back to the strict policy rather than producing a malformed
// header (#64).
func ValidHostSyntax(host string) bool {
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// MatchRedirectURI reports whether presented matches one of the client's
// registered redirect URIs. Matching is exact string equality, with the single
// OAuth 2.1 / RFC 8252 §7.3 allowance: for a loopback http URI the PORT may
// vary between registration and authorization (a native client binds an
// ephemeral port), while scheme, host, and path must still match exactly. No
// request-supplied URI outside the registered set is ever honored (SPEC-0007
// scenario "Redirect to an unregistered URI").
func MatchRedirectURI(registered []string, presented string) bool {
	for _, reg := range registered {
		if reg == presented {
			return true
		}
	}
	// Loopback port flexibility only.
	p, err := url.Parse(presented)
	if err != nil || p.Scheme != "http" || !isLoopbackHost(p.Hostname()) || p.Fragment != "" {
		return false
	}
	for _, reg := range registered {
		r, err := url.Parse(reg)
		if err != nil || r.Scheme != "http" || !isLoopbackHost(r.Hostname()) {
			continue
		}
		if r.Hostname() == p.Hostname() && r.Path == p.Path && r.RawQuery == p.RawQuery {
			return true
		}
	}
	return false
}

// --- Secrets -----------------------------------------------------------------

// newSecret mints a 32-byte high-entropy URL-safe secret with a legibility
// prefix ("cairn_ac_", "cairn_at_", "cairn_rt_") so leaked strings are
// identifiable in scanners without weakening entropy.
func newSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: mint secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret maps a raw secret to the lowercase-hex SHA-256 stored at rest, so
// lookups are constant-cost and a database disclosure yields nothing
// replayable (SPEC-0007 REQ "Database Operation Standards").
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// --- Domain records -----------------------------------------------------------

// Client is a dynamically registered OAuth client (RFC 7591). All Cairn
// clients are public clients authenticated by PKCE (token_endpoint_auth_method
// "none"), per the MCP authorization spec.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// TokenSet is one issue/rotation result: the raw secrets (shown exactly once)
// plus the metadata the token response renders.
type TokenSet struct {
	GrantID         string
	AccessToken     string
	RefreshToken    string
	AccessExpiresIn int64 // seconds
	Scope           string
}

// Identity is the resolved principal behind a valid access token: the human
// subject the grant binds (agents inherit the human's reach, ADR-0004
// subject-vs-actor) plus the granted scope subset.
type Identity struct {
	ActorID  string
	ClientID string
	GrantID  string
	Scopes   []string
	// ExpiresAt is the access token's own expiry, exposed so a bearer-token
	// adapter (e.g. the MCP transport's auth.TokenVerifier, SPEC-0007) can
	// populate its own token-info expiration without a second lookup.
	ExpiresAt time.Time
}
