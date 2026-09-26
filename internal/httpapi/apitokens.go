package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/operator"
	"github.com/stump-wtf/cairn/internal/user"
)

// apiTokenForm is the entry grammar every CAIRN_API_TOKENS error names, so a
// refused entry always tells the operator what to write instead.
const apiTokenForm = "secret:<user>[:agent|:human], where <user> is <issuer>|<subject> or a verified email"

// APIToken is one CAIRN_API_TOKENS entry: an opaque high-entropy secret that
// acts as an operator's user (SPEC-0023 REQ "Static API Tokens Act as an
// Operator's User"). A credential the user cannot see or revoke must not be
// able to act as them, so the only user an entry may name is an operator
// automating their own account; everyone else mints personal access tokens.
//
// Governing: ADR-0004 (the static-token seam), ADR-0029 section 7, SPEC-0023
// REQ "Static API Tokens Act as an Operator's User".
type APIToken struct {
	// Secret is the bearer value the client presents. TokenAuthenticator
	// holds it only as a SHA-256 digest; no error or log line ever carries it.
	Secret string
	// User is the entry's <user> as configured: "<issuer>|<subject>" or a
	// verified email. It names the user; it is never itself an identity.
	User string
	// IsAgent is true unless the entry ends in ":human". An agent token gets
	// the ADR-0004 agent scopes and never sharing:manage.
	IsAgent bool
	// Position is the entry's 1-based position among the non-empty
	// CAIRN_API_TOKENS entries: what every error and the Settings page call
	// it, since the secret is never shown.
	Position int
	// UserID is the user the entry resolved to at boot (ResolveAPITokens).
	// An entry without one never authenticates.
	UserID string
}

// ParseAPITokens parses CAIRN_API_TOKENS: comma-separated
// `secret:<user>[:agent|:human]` entries, where <user> is an
// "<issuer>|<subject>" (split at its first "|", as CAIRN_OPERATORS is) or an
// email. The role defaults to agent. Whitespace around entries and fields is
// trimmed, and an empty value yields no tokens (the bearer surface then
// rejects every token, failing closed).
//
// Every malformed entry is a boot error naming its position and the entry
// form, and never any part of the entry itself: a secret typed into the
// wrong field would otherwise land in the log. That includes the legacy
// free-form `secret:actor[:role]` entries, refused from this release with no
// grace period. Resolving each entry to an operator's user needs the
// database and happens in ResolveAPITokens.
//
// Governing: ADR-0012 (config from the environment), ADR-0029 section 7,
// SPEC-0023 REQ "Static API Tokens Act as an Operator's User".
func ParseAPITokens(raw string) ([]APIToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var (
		tokens []APIToken
		seen   = map[[sha256.Size]byte]int{}
		pos    int
	)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		pos++
		t, err := parseAPIToken(entry)
		if err != nil {
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w", pos, err)
		}
		t.Position = pos
		key := sha256.Sum256([]byte(t.Secret))
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: same secret as entry %d", pos, first)
		}
		seen[key] = pos
		tokens = append(tokens, t)
	}
	return tokens, nil
}

// parseAPIToken parses one trimmed, non-empty entry. Its errors never quote
// the entry.
func parseAPIToken(entry string) (APIToken, error) {
	secret, ref, ok := strings.Cut(entry, ":")
	secret, ref = strings.TrimSpace(secret), strings.TrimSpace(ref)
	if !ok || secret == "" || ref == "" {
		return APIToken{}, fmt.Errorf("want %s", apiTokenForm)
	}
	t := APIToken{Secret: secret, IsAgent: true}
	issuer, subject, isIdentity := strings.Cut(ref, "|")
	// The role is an optional last ":" field. An identity's issuer is a URL
	// and holds colons of its own, so only a colon inside the subject can
	// start the role; a subject may itself hold a colon, so only the two
	// role words are taken as one.
	tail := ref
	if isIdentity {
		tail = subject
	}
	if i := strings.LastIndex(tail, ":"); i >= 0 {
		switch role := strings.TrimSpace(tail[i+1:]); role {
		case "agent", "human":
			t.IsAgent = role == "agent"
			ref = strings.TrimSpace(ref[:len(ref)-len(tail)+i])
			issuer, subject, isIdentity = strings.Cut(ref, "|")
		default:
			// An identity keeps an unknown last field in its subject, since
			// a subject may hold a colon, except one that can only be a
			// mistyped role: a role word in the wrong case, or nothing after
			// a trailing colon. Kept, it would fail at boot as a user who
			// never signed in, which misdiagnoses the typo.
			mistyped := role == "" || strings.EqualFold(role, "agent") || strings.EqualFold(role, "human")
			if (isIdentity && mistyped) || (!isIdentity && validTokenEmail(strings.TrimSpace(ref[:i]))) {
				return APIToken{}, fmt.Errorf("the role must be agent or human; want %s", apiTokenForm)
			}
		}
	}
	switch {
	case isIdentity:
		if strings.TrimSpace(issuer) == "" || strings.TrimSpace(subject) == "" {
			return APIToken{}, fmt.Errorf("an identity needs an issuer and a subject; want %s", apiTokenForm)
		}
		t.User = strings.TrimSpace(issuer) + "|" + strings.TrimSpace(subject)
	case validTokenEmail(ref):
		t.User = user.NormalizeEmail(ref)
	default:
		// A bare name: the free-form actor every entry was before
		// SPEC-0023. It named whatever string the operator chose, so it
		// cannot be read as a user, and there is no grace period.
		return APIToken{}, fmt.Errorf("legacy secret:actor entries are no longer accepted; name an operator's user as %s", apiTokenForm)
	}
	return t, nil
}

// validTokenEmail reports whether s has the shape of an email: one "@" with
// something on both sides and no spaces or colons. Whether it names a user
// is decided at boot, against verified primary emails only.
func validTokenEmail(s string) bool {
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsAny(s, " \t:")
}

// Errors ResolveAPITokens wraps with the entry's position.
var (
	errTokenNoUser      = errors.New("no user; the operator must sign in once before a token can name them")
	errTokenSuspended   = errors.New("user is suspended")
	errTokenNotOperator = errors.New("user is not an operator")
)

// resolveAPITokens binds every entry to the existing user it names and
// refuses any whose user is not an operator (listed in CAIRN_OPERATORS by
// one of their sign-in identities). Nothing is created: a token can only name
// someone who has already signed in. The first failure is returned, naming
// the entry's position and never its secret.
//
// Governing: ADR-0029 section 7, SPEC-0023 REQ "Static API Tokens Act as an
// Operator's User" ("Token naming a non-operator").
func resolveAPITokens(ctx context.Context, users *user.Store, ops *operator.Service, tokens []APIToken) ([]APIToken, error) {
	out := make([]APIToken, 0, len(tokens))
	for _, t := range tokens {
		if users == nil || ops == nil {
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: tokens need the database to resolve their user", t.Position)
		}
		var (
			u   *user.User
			err error
		)
		issuer, subject, isIdentity := strings.Cut(t.User, "|")
		if isIdentity {
			u, err = users.FindByIdentity(ctx, issuer, subject)
		} else {
			u, err = users.FindByVerifiedEmail(ctx, t.User)
		}
		switch {
		case errors.Is(err, user.ErrNotFound) && isIdentity && strings.Contains(subject, ":"):
			// The parser keeps an unknown ":<word>" in the subject, so a
			// mistyped role such as ":admin" surfaces here. Say so, without
			// quoting the entry.
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w (its subject holds a colon; if the last field is meant as a role, it must be agent or human)", t.Position, errTokenNoUser)
		case errors.Is(err, user.ErrNotFound):
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w", t.Position, errTokenNoUser)
		case err != nil:
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: resolve user: %w", t.Position, err)
		case u.SuspendedAt != nil:
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w", t.Position, errTokenSuspended)
		}
		isOp, err := ops.IsOperatorUser(ctx, u.ID)
		if err != nil {
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w", t.Position, err)
		}
		if !isOp {
			return nil, fmt.Errorf("CAIRN_API_TOKENS entry %d: %w", t.Position, errTokenNotOperator)
		}
		t.UserID = u.ID
		out = append(out, t)
	}
	return out, nil
}

// ResolveAPITokens resolves the configured CAIRN_API_TOKENS entries to their
// operators' users and installs them on the bearer surface. cairnd calls it
// once at boot, before serving, and fails to start on its error; until it
// succeeds no static token authenticates. It is a no-op when no tokens are
// configured.
func (s *Server) ResolveAPITokens(ctx context.Context) error {
	if len(s.cfg.APITokens) == 0 {
		return nil
	}
	if s.staticTokens == nil {
		return errors.New("CAIRN_API_TOKENS: this server has no static token surface")
	}
	resolved, err := resolveAPITokens(ctx, s.users, s.ops, s.cfg.APITokens)
	if err != nil {
		return err
	}
	s.staticTokens.install(resolved)
	return nil
}

// TokenAuthenticator verifies an `Authorization: Bearer <token>` credential
// against the resolved CAIRN_API_TOKENS entries. A raw bearer string is NEVER
// trusted as an actor id: it must hash to a registered secret, and that
// secret acts as the user it was resolved to at boot and nobody else, so no
// request field can choose who it acts as. Secrets are held only as SHA-256
// digests, and lookup hashes the presented token to a fixed-width key so
// verification cost does not vary with which token matched.
//
// The channel is server-derived to `via API` (SPEC-0002 "Channel is
// server-derived"). Every request re-reads the user, so a suspended
// operator's token stops at once (SPEC-0023 "Suspending a user offboards
// them").
//
// Governing: ADR-0004 (token seam), SPEC-0002 (server-derived channel),
// ADR-0029 section 7, SPEC-0023 REQ "Static API Tokens Act as an Operator's
// User" ("Token cannot impersonate").
type TokenAuthenticator struct {
	// grants maps sha256(secret) → the credential. The digest key means the
	// map stores no plaintext secret. It is swapped whole by install.
	grants atomic.Pointer[map[[sha256.Size]byte]APIToken]
	// users re-reads each token's user per request (nil on storeless
	// wirings, whose principals render the configured User instead).
	users *user.Store
}

// NewTokenAuthenticator builds a TokenAuthenticator over already-resolved
// credentials. An entry without a UserID is left out, and a nil or empty set
// rejects every bearer token: the fail-closed default.
func NewTokenAuthenticator(tokens []APIToken) *TokenAuthenticator {
	a := &TokenAuthenticator{}
	a.install(tokens)
	return a
}

// install replaces the credential set with the resolved entries of tokens.
func (a *TokenAuthenticator) install(tokens []APIToken) {
	grants := make(map[[sha256.Size]byte]APIToken, len(tokens))
	for _, t := range tokens {
		if t.UserID == "" {
			continue
		}
		grants[sha256.Sum256([]byte(t.Secret))] = t
	}
	a.grants.Store(&grants)
}

// forUser lists the resolved entries acting as userID, by position, with
// the secrets blanked: what the user's Settings page shows.
func (a *TokenAuthenticator) forUser(userID string) []APIToken {
	if a == nil || userID == "" {
		return nil
	}
	var out []APIToken
	if g := a.grants.Load(); g != nil {
		for _, t := range *g {
			if t.UserID == userID {
				t.Secret = ""
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// Authenticate implements Authenticator. An absent bearer, an unregistered
// secret, or a registered one whose user is gone or suspended is
// errs.ErrUnauthorized. A registered secret acts as its resolved user with
// the scopes its role grants, never as whatever the caller typed.
func (a *TokenAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errs.ErrUnauthorized
	}
	var (
		grant APIToken
		ok    bool
	)
	if g := a.grants.Load(); g != nil {
		grant, ok = (*g)[sha256.Sum256([]byte(token))]
	}
	if !ok || grant.UserID == "" {
		// Touch a constant-time compare against a fixed sentinel so an unknown
		// token does not resolve visibly faster than a byte-mismatched one.
		subtle.ConstantTimeCompare([]byte(token), []byte(token))
		return nil, errs.ErrUnauthorized
	}
	scopes := humanScopes()
	if grant.IsAgent {
		scopes = agentScopes()
	}
	p := &Principal{
		ActorID: grant.User,
		UserID:  grant.UserID,
		Channel: artifact.ChannelAPI,
		IsAgent: grant.IsAgent,
		Scopes:  scopes,
	}
	if a.users != nil {
		u, err := a.users.Get(r.Context(), grant.UserID)
		if err != nil || u.SuspendedAt != nil {
			return nil, errs.ErrUnauthorized
		}
		p.ActorID = u.Actor
	}
	return p, nil
}
