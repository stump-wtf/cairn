// Package authprovider abstracts the human login providers behind one
// interface (SPEC-0012): a provider knows how to start its login (produce the
// redirect to its authorization endpoint) and how to finish one (turn the
// callback's code into a verified identity). Everything flow-shaped — routes,
// the shared state cookie, session establishment, CSRF — lives in the httpapi
// dispatcher and is deliberately NOT part of this interface, so Pocket ID
// (OIDC) and GitHub (plain OAuth 2.0 + REST profile fetch) can differ in
// verification while feeding the same session establishment.
//
// Governing: SPEC-0012, ADR-0017.
package authprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
)

// GitHubIssuer is the iss recorded on sessions a GitHub login established.
const GitHubIssuer = "https://github.com"

// Identity is the verified outcome of a finished login: who issued it, the
// subject that issuer asserts, and the actor id the session is keyed on (the
// primary verified email, matching what the OIDC flow keys sessions on).
type Identity struct {
	Issuer  string
	Subject string
	Actor   string
}

// Provider is one human login provider. StartLogin returns the authorization
// redirect URL for a fresh login (the caller stashes its own per-login state —
// including the provider id — in the shared state cookie before redirecting).
// FinishLogin validates the callback against the same stashed state and
// exchanges the code for a verified Identity. Implementations must treat
// every rejection as terminal: no partial identity, nothing persisted.
type Provider interface {
	// ID is the registry key the ?provider= parameter selects ("github").
	ID() string
	// DisplayName is the human-facing name the login page renders.
	DisplayName() string
	// StartLogin mints the authorization redirect URL for this login attempt.
	// state is the single-use anti-CSRF value the caller also stashes in the
	// state cookie and requires back at the callback.
	StartLogin(state, next string) (redirectURL string, err error)
	// FinishLogin exchanges the callback's code and verifies the provider's
	// answer, returning the verified Identity. st carries the state that was
	// stashed at StartLogin; implementations must reject when st.State does
	// not match the callback's state parameter (checked here so no caller can
	// forget it).
	FinishLogin(ctx context.Context, st State, r *http.Request) (Identity, error)
}

// State is the per-login state the dispatcher stashes in the shared state
// cookie and hands back at the callback. It reuses the OIDC state cookie's
// TTL and single-use clearing.
type State struct {
	State    string `json:"s"`
	Provider string `json:"p,omitempty"`
	Next     string `json:"x,omitempty"`
}

// GitHubProvider implements Provider against GitHub's OAuth 2.0 endpoints and
// REST API. OAuth carries the client credentials and the endpoint (a field so
// tests can point it at an httptest fake); APIBase likewise defaults to
// github.com and is overridable in tests. The access token lives only inside
// FinishLogin and is dropped when it returns — never persisted or logged.
type GitHubProvider struct {
	OAuth   *oauth2.Config // Scopes must be "read:user user:email".
	APIBase string         // default https://api.github.com
}

// NewGitHubProvider builds the GitHub provider from client credentials and
// the instance's public origin (the redirect URI is always BaseURL +
// /auth/callback, derived — never separately configured).
func NewGitHubProvider(clientID, clientSecret, baseURL string) *GitHubProvider {
	return &GitHubProvider{
		OAuth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  baseURL + "/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://github.com/login/oauth/authorize",
				TokenURL: "https://github.com/login/oauth/access_token",
			},
			Scopes: []string{"read:user", "user:email"},
		},
		APIBase: "https://api.github.com",
	}
}

// ID implements Provider.
func (g *GitHubProvider) ID() string { return "github" }

// DisplayName implements Provider.
func (g *GitHubProvider) DisplayName() string { return "GitHub" }

// StartLogin implements Provider.
func (g *GitHubProvider) StartLogin(state, _ string) (string, error) {
	if state == "" {
		return "", fmt.Errorf("github: empty state")
	}
	return g.OAuth.AuthCodeURL(state), nil
}

// githubUser is the slice of GET /user this provider keys identity on.
type githubUser struct {
	Login string `json:"login"`
}

// githubEmail is one entry of GET /user/emails.
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// FinishLogin implements Provider. Checks the stashed state against the
// callback's state parameter BEFORE any token exchange, exchanges the code
// for an access token (dropped at return), fetches the profile and emails,
// and requires a primary VERIFIED email — an unverified or missing one is a
// rejection, not an actor id.
func (g *GitHubProvider) FinishLogin(ctx context.Context, st State, r *http.Request) (Identity, error) {
	if st.State == "" || st.State != r.URL.Query().Get("state") {
		return Identity{}, fmt.Errorf("github: state mismatch")
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return Identity{}, fmt.Errorf("github: missing code")
	}
	tok, err := g.OAuth.Exchange(ctx, code)
	if err != nil {
		return Identity{}, fmt.Errorf("github: token exchange: %w", err)
	}
	client := g.OAuth.Client(ctx, tok)
	client.Timeout = 0 // the context bounds this; oauth2 sets no default

	var user githubUser
	if err := getJSON(client, g.api("/user"), &user); err != nil {
		return Identity{}, fmt.Errorf("github: fetch user: %w", err)
	}
	if user.Login == "" {
		return Identity{}, fmt.Errorf("github: user has no login")
	}
	var emails []githubEmail
	if err := getJSON(client, g.api("/user/emails"), &emails); err != nil {
		return Identity{}, fmt.Errorf("github: fetch emails: %w", err)
	}
	actor := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			actor = strings.ToLower(strings.TrimSpace(e.Email))
			break
		}
	}
	if actor == "" {
		return Identity{}, fmt.Errorf("github: no primary verified email")
	}
	// tok (the GitHub access token) is deliberately dropped here: it
	// authenticated nothing but this one profile fetch, and SPEC-0012's token
	// containment forbids persisting or logging it.
	return Identity{Issuer: GitHubIssuer, Subject: user.Login, Actor: actor}, nil
}

// getJSON fetches url as the authenticated user and decodes the JSON body.
// The vnd.github+json Accept header is what makes /user/emails answer with
// JSON rather than the browser variant.
func getJSON(client *http.Client, url string, v any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// api joins the configured API base with path.
func (g *GitHubProvider) api(path string) string { return g.APIBase + path }

// ValidateNext reports whether next is an acceptable post-login destination:
// empty (the default) or a same-origin absolute path. Anything with a scheme
// or netloc — the open-redirect shapes — is rejected, so the state cookie's
// stashed destination is the only post-login navigation.
func ValidateNext(next string) bool {
	if next == "" {
		return true
	}
	u, err := url.Parse(next)
	if err != nil {
		return false
	}
	return u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/")
}
