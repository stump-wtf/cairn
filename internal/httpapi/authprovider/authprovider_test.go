package authprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

// fakeGitHub stands in for github.com's OAuth + REST endpoints. The oauth2
// Config points its TokenURL at the fake, and APIBase at the fake's REST
// surface, so FinishLogin runs entirely against local httptest servers — no
// network-dependent tests.
func fakeGitHub(t *testing.T, tokenResp string, tokenStatus int, user string, emails string) (*GitHubProvider, *int, *int) {
	t.Helper()
	exchanges := 0
	emailFetches := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tokenStatus)
		_, _ = w.Write([]byte(tokenResp))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("profile fetch carried no Authorization header")
		}
		_, _ = w.Write([]byte(user))
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		emailFetches++
		_, _ = w.Write([]byte(emails))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	apiURL, _ := url.Parse(srv.URL)
	g := &GitHubProvider{
		OAuth: &oauth2.Config{
			ClientID:     "cid",
			ClientSecret: "csecret",
			RedirectURL:  "https://cairn.example/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  srv.URL + "/login/oauth/authorize",
				TokenURL: srv.URL + "/login/oauth/access_token",
			},
			Scopes: []string{"read:user", "user:email"},
		},
		APIBase: apiURL.Scheme + "://" + apiURL.Host,
	}
	return g, &exchanges, &emailFetches
}

func callback(state, code string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	q := r.URL.Query()
	q.Set("state", state)
	q.Set("code", code)
	r.URL.RawQuery = q.Encode()
	return r
}

const okEmails = `[{"email":"jo@example.com","primary":true,"verified":true}]`

func TestGitHubFinishLoginHappyPath(t *testing.T) {
	g, exchanges, _ := fakeGitHub(t,
		`{"access_token":"tok","token_type":"bearer"}`, http.StatusOK,
		`{"login":"octocat"}`, okEmails)
	id, err := g.FinishLogin(context.Background(), State{State: "s1"}, callback("s1", "c1"))
	if err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	if id.Issuer != GitHubIssuer || id.Subject != "octocat" || id.Actor != "jo@example.com" {
		t.Errorf("identity = %+v", id)
	}
	if *exchanges != 1 {
		t.Errorf("exchanges = %d, want 1", *exchanges)
	}
}

func TestGitHubFinishLoginStateMismatchBeforeExchange(t *testing.T) {
	g, exchanges, _ := fakeGitHub(t,
		`{"access_token":"tok"}`, http.StatusOK, `{"login":"x"}`, okEmails)
	if _, err := g.FinishLogin(context.Background(), State{State: "expected"}, callback("WRONG", "c1")); err == nil {
		t.Fatal("state mismatch accepted")
	}
	if *exchanges != 0 {
		t.Errorf("token exchange ran %d times on a state mismatch; must reject BEFORE exchanging", *exchanges)
	}
}

func TestGitHubFinishLoginRequiresPrimaryVerifiedEmail(t *testing.T) {
	for name, emails := range map[string]string{
		"unverified primary": `[{"email":"jo@example.com","primary":true,"verified":false}]`,
		"no primary":         `[{"email":"jo@example.com","primary":false,"verified":true}]`,
		"empty":              `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			g, _, emailFetches := fakeGitHub(t,
				`{"access_token":"tok"}`, http.StatusOK, `{"login":"octocat"}`, emails)
			if _, err := g.FinishLogin(context.Background(), State{State: "s"}, callback("s", "c")); err == nil {
				t.Fatalf("accepted %s", name)
			}
			if *emailFetches == 0 {
				t.Error("emails endpoint never consulted")
			}
		})
	}
}

func TestGitHubStartLoginRequiresState(t *testing.T) {
	g := &GitHubProvider{OAuth: &oauth2.Config{}}
	if _, err := g.StartLogin("", ""); err == nil {
		t.Fatal("empty state accepted")
	}
}

func TestValidateNext(t *testing.T) {
	cases := map[string]bool{
		"":           true,
		"/bin":       true,
		"/settings":  true,
		"https://x":  false,
		"//evil.com": false,
		"/../escape": true,  // path-relative; server routing confines it
		"http://x/y": false, //nolint:goconst // literal pair, not a repeated string
	}
	for next, want := range cases {
		if got := ValidateNext(next); got != want {
			t.Errorf("ValidateNext(%q) = %v, want %v", next, got, want)
		}
	}
}

// Compile-time shape checks the JSON contract the handler round-trips.
func TestStateRoundTrip(t *testing.T) {
	b, _ := json.Marshal(State{State: "s", Provider: "github", Next: "/bin"})
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Provider != "github" || st.State != "s" || st.Next != "/bin" {
		t.Errorf("round trip = %+v", st)
	}
}
