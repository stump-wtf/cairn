package httpapi

// Integration tests for the operator profile (SPEC-0023 REQ "Operator and
// User Profiles" and REQ "Operator Surfaces Bound Tenant Data and Never Read
// It"). Each runs against real Postgres through the real sign-in routes, with
// the fake OIDC IdP from oidc_integration_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/operator"
	"github.com/stump-wtf/cairn/internal/store"
)

const (
	opSubject     = "op-subject"
	opEmail       = "op@example.com"
	victimSubject = "victim-subject"
	victimEmail   = "victim@example.com"
)

// operatorServer stands up the adapter with OIDC wired against idp and the
// operator profile parsed from operators and group, exactly as cairnd does.
// The dev bearer shortcut is on so a test can seed artifacts as an owner and
// prove a bearer caller is refused the operator surface.
func operatorServer(t *testing.T, idp *fakeIdP, operators, group string) *httptest.Server {
	t.Helper()
	return operatorServerWith(t, idp, operators, group, true)
}

// operatorServerWith is operatorServer with the dev bearer shortcut chosen.
// A test that proves a real token stops authenticating turns it off: with it
// on, any rejected secret would still authenticate as itself.
func operatorServerWith(t *testing.T, idp *fakeIdP, operators, group string, devBearer bool) *httptest.Server {
	t.Helper()
	set, err := operator.Parse(operators, group)
	if err != nil {
		t.Fatalf("operator.Parse: %v", err)
	}
	pool := newTestPool(t)
	st := store.New(pool, objectstore.NewMemory(), storeOpts())
	cfg := oidcConfig()
	cfg.DevInsecureBearerAuth = devBearer
	cfg.OIDCIssuer = idp.srv.URL
	cfg.OIDCClientID = testOIDCClientID
	cfg.OIDCClientSecret = "test-secret"
	cfg.Operators = set
	api := New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := api.EnableOIDC(context.Background()); err != nil {
		t.Fatalf("EnableOIDC: %v", err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// signInAs signs a fresh browser in as the given OIDC identity.
func signInAs(t *testing.T, srv *httptest.Server, idp *fakeIdP, subject, email string, groups any) *http.Client {
	t.Helper()
	idp.setIdentity(subject, email, true)
	idp.setGroups(groups)
	return signInOIDC(t, srv, idp)
}

// statusOf issues a GET with client (a nil client is anonymous) and returns
// the status and body.
func statusOf(t *testing.T, client *http.Client, u string) (int, string) {
	t.Helper()
	if client == nil {
		client = newJarClient()
	}
	return getHTMLClient(t, client, u)
}

// operatorDirectory fetches GET /v1/operator/directory as client.
func operatorDirectory(t *testing.T, srv *httptest.Server, client *http.Client) (directoryResponse, string) {
	t.Helper()
	status, body := statusOf(t, client, srv.URL+"/v1/operator/directory")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/operator/directory = %d, want 200 (%s)", status, body)
	}
	var dir directoryResponse
	if err := json.Unmarshal([]byte(body), &dir); err != nil {
		t.Fatalf("decode directory: %v", err)
	}
	return dir, body
}

// directoryUserID finds the user whose rendered actor is actor.
func directoryUserID(t *testing.T, dir directoryResponse, actor string) string {
	t.Helper()
	for _, u := range dir.Users {
		if u.Actor == actor {
			return u.ID
		}
	}
	t.Fatalf("no directory user renders as %q", actor)
	return ""
}

// suspend posts to /v1/operator/suspensions as client, with the CSRF header.
func suspend(t *testing.T, srv *httptest.Server, client *http.Client, userID string, suspended bool, reason string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"user_id": userID, "suspended": suspended, "reason": reason})
	return sessionJSON(t, srv.URL, client, http.MethodPost, "/v1/operator/suspensions", bytes.NewReader(b))
}

// operatorRoutes are every operator surface, as (method, path).
var operatorRoutes = [][2]string{
	{http.MethodGet, "/operator"},
	{http.MethodPost, "/operator/suspensions"},
	{http.MethodGet, "/v1/operator/directory"},
	{http.MethodPost, "/v1/operator/suspensions"},
}

// assertOperatorRoutes404 proves every operator route answers the uniform
// 404 to client (nil = anonymous), with token as a bearer when set.
func assertOperatorRoutes404(t *testing.T, srv *httptest.Server, client *http.Client, token, who string) {
	t.Helper()
	if client == nil {
		client = newJarClient()
	}
	for _, rt := range operatorRoutes {
		req, _ := http.NewRequest(rt[0], srv.URL+rt[1], strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if csrf := cookieValue(t, client, srv.URL, csrfCookieName); csrf != "" {
			req.Header.Set(csrfHeaderName, csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rt[0], rt[1], err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %s %s = %d, want 404", who, rt[0], rt[1], resp.StatusCode)
		}
	}
}

// SPEC-0023 "Operator by provenance": with CAIRN_OPERATORS naming an
// identity, that identity's browser session gets the console, and its Bin
// still shows only its own artifacts.
func TestIntegrationOperatorByProvenance(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServer(t, idp, idp.srv.URL+"|"+opSubject, "")
	victimArt := seedArtifact(t, srv, victimEmail)
	ownArt := seedArtifact(t, srv, opEmail)

	op := signInAs(t, srv, idp, opSubject, opEmail, nil)
	status, body := statusOf(t, op, srv.URL+"/operator")
	if status != http.StatusOK || !strings.Contains(body, "Operator console") {
		t.Fatalf("GET /operator as the operator = %d, want 200 with the console", status)
	}
	dir, raw := operatorDirectory(t, srv, op)
	if len(dir.Users) < 2 || dir.Teams == nil {
		t.Fatalf("directory = %+v, want both users and a teams array", dir)
	}
	for _, leak := range []string{victimArt, ownArt, "Owned", "# owned"} {
		if strings.Contains(raw, leak) || strings.Contains(body, leak) {
			t.Errorf("an operator surface leaks %q", leak)
		}
	}

	bin := sessionBinIDs(t, srv, op)
	if !bin[ownArt] || bin[victimArt] || len(bin) != 1 {
		t.Fatalf("operator Bin = %v, want only their own artifact %s", bin, ownArt)
	}

	_, settings := statusOf(t, op, srv.URL+"/settings")
	if !strings.Contains(settings, `href="/operator"`) || !strings.Contains(settings, idp.srv.URL+"|"+opSubject) {
		t.Error("Settings does not show the operator's sign-in identity and console link")
	}

	// Any other signed-in user, a bearer token naming the operator, and an
	// anonymous caller all get the uniform 404.
	other := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	assertOperatorRoutes404(t, srv, other, "", "non-operator session")
	assertOperatorRoutes404(t, srv, nil, opEmail, "bearer naming the operator")
	assertOperatorRoutes404(t, srv, nil, "", "anonymous")
	if _, s := statusOf(t, other, srv.URL+"/settings"); strings.Contains(s, `href="/operator"`) {
		t.Error("a non-operator's Settings links the console")
	}
}

// SPEC-0023 "No operator configured": every operator route answers 404 and
// every user surface works normally.
func TestIntegrationNoOperatorConfigured(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServer(t, idp, "", "")
	own := seedArtifact(t, srv, opEmail)
	client := signInAs(t, srv, idp, opSubject, opEmail, []string{"cairn-ops"})

	assertOperatorRoutes404(t, srv, client, "", "signed-in user, no operator configured")
	if !sessionBinIDs(t, srv, client)[own] {
		t.Error("the Bin stopped working with no operator configured")
	}
	if status, body := statusOf(t, client, srv.URL+"/settings"); status != http.StatusOK || strings.Contains(body, `href="/operator"`) {
		t.Errorf("Settings = %d (console linked: %v), want 200 and no console link", status, strings.Contains(body, `href="/operator"`))
	}
}

// CAIRN_OPERATOR_GROUP: a session is an operator's when the OIDC groups claim
// carried the group at sign-in. The groups scope is requested only while a
// group is configured.
func TestIntegrationOperatorByGroup(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServer(t, idp, "", "cairn-ops")

	_, loc := doOIDCLogin(t, srv, newJarClient(), "")
	if scope := loc.Query().Get("scope"); !strings.Contains(" "+scope+" ", " groups ") {
		t.Errorf("authorization scope = %q, want it to request groups", scope)
	}
	plain := operatorServer(t, idp, idp.srv.URL+"|x", "")
	if _, loc := doOIDCLogin(t, plain, newJarClient(), ""); strings.Contains(loc.Query().Get("scope"), "groups") {
		t.Error("groups scope requested with no operator group configured")
	}

	member := signInAs(t, srv, idp, opSubject, opEmail, []any{"staff", "cairn-ops"})
	if status, _ := statusOf(t, member, srv.URL+"/operator"); status != http.StatusOK {
		t.Fatalf("group member GET /operator = %d, want 200", status)
	}
	single := signInAs(t, srv, idp, "single-sub", "single@example.com", "cairn-ops")
	if status, _ := statusOf(t, single, srv.URL+"/operator"); status != http.StatusOK {
		t.Fatalf("a groups claim sent as one string: GET /operator = %d, want 200", status)
	}
	for name, groups := range map[string]any{"no claim": nil, "other groups": []any{"staff", "cairn-ops-2"}} {
		slug := strings.ReplaceAll(name, " ", "-")
		c := signInAs(t, srv, idp, "sub-"+slug, slug+"@example.com", groups)
		assertOperatorRoutes404(t, srv, c, "", name)
	}
}

// SPEC-0023 "Operator opens a private artifact's link": operator status never
// widens a read. The operator gets exactly what any other non-owner gets for
// another user's private artifact, on every surface; the uniform 404 itself
// is authorizeRead's (#182), which this test then holds for the operator too.
func TestIntegrationOperatorReadsNoMoreThanAnyone(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServer(t, idp, idp.srv.URL+"|"+opSubject, "")
	id := seedArtifact(t, srv, victimEmail)
	owner := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	resp := sessionJSON(t, srv.URL, owner, http.MethodPatch, "/v1/artifacts/"+id+"/policy", strings.NewReader(`{"visibility":"private"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("make private = %d, want 200", resp.StatusCode)
	}

	op := signInAs(t, srv, idp, opSubject, opEmail, nil)
	bystander := signInAs(t, srv, idp, "bystander-sub", "bystander@example.com", nil)
	for _, path := range []string{"/v1/artifacts/" + id, "/v1/artifacts/" + id + "/body", "/" + id, "/v1/artifacts/" + id + "/annotations"} {
		opStatus, opBody := statusOf(t, op, srv.URL+path)
		byStatus, byBody := statusOf(t, bystander, srv.URL+path)
		anonStatus, _ := statusOf(t, nil, srv.URL+path)
		if opStatus != byStatus || opStatus != anonStatus {
			t.Errorf("GET %s: operator %d, bystander %d, anonymous %d; want the operator treated as any non-owner", path, opStatus, byStatus, anonStatus)
		}
		if opStatus == http.StatusNotFound && opBody != byBody {
			t.Errorf("GET %s: the operator's 404 differs from a bystander's", path)
		}
	}
	if sessionBinIDs(t, srv, op)[id] {
		t.Fatal("another user's private artifact is in the operator's Bin")
	}
}

// SPEC-0023 "Suspending a user offboards them": U's sessions and personal
// access tokens stop authenticating on the next request, U cannot sign in
// again, and U reads the audit row after reinstatement.
func TestIntegrationSuspendingUserOffboards(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServerWith(t, idp, idp.srv.URL+"|"+opSubject, "", false)
	op := signInAs(t, srv, idp, opSubject, opEmail, nil)
	victim := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	bystander := signInAs(t, srv, idp, "bystander-sub", "bystander@example.com", nil)
	token := createPAT(t, srv, victim, "laptop", []string{scopeArtifactsWrite}, false).Token
	byToken := createPAT(t, srv, bystander, "laptop", []string{scopeArtifactsWrite}, false).Token

	dir, _ := operatorDirectory(t, srv, op)
	victimID := directoryUserID(t, dir, victimEmail)

	// The suspension route is CSRF-guarded like every browser write.
	b, _ := json.Marshal(map[string]any{"user_id": victimID, "suspended": true, "reason": "spam"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/operator/suspensions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	noCSRF, err := op.Do(req)
	if err != nil {
		t.Fatalf("POST without CSRF: %v", err)
	}
	noCSRF.Body.Close()
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("suspension without the CSRF header = %d, want 403", noCSRF.StatusCode)
	}
	// A reason is required, and a refusal changes nothing.
	resp := suspend(t, srv, op, victimID, true, "  ")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("suspension without a reason = %d, want 400", resp.StatusCode)
	}
	if whoamiActor(t, srv, victim) != victimEmail {
		t.Fatal("a refused suspension signed the user out")
	}

	resp = suspend(t, srv, op, victimID, true, "posting spam")
	var out suspensionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode suspension: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !out.Suspended || out.Revoked.Sessions != 1 || out.Revoked.PersonalTokens != 1 || out.AuditID == 0 {
		t.Fatalf("suspend = %d %+v, want 200, suspended, one session and one token revoked", resp.StatusCode, out)
	}

	// Next request: the session and the token no longer authenticate.
	if status, _ := statusOf(t, victim, srv.URL+"/whoami"); status != http.StatusSeeOther {
		t.Errorf("suspended user's session: GET /whoami = %d, want 303 to sign-in", status)
	}
	r := do(t, http.MethodGet, srv.URL+"/v1/whoami", token, nil, "")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("suspended user's PAT: GET /v1/whoami = %d, want 401", r.StatusCode)
	}
	// Nor can they sign in again, and no session is minted.
	idp.setIdentity(victimSubject, victimEmail, true)
	again := newJarClient()
	_, loc := doOIDCLogin(t, srv, again, "")
	idp.setNonce(loc.Query().Get("nonce"))
	cb := doOIDCCallback(t, srv, again, loc.Query().Get("state"))
	cb.Body.Close()
	if cb.StatusCode != http.StatusForbidden || cookieValue(t, again, srv.URL, sessionCookieName) != "" {
		t.Errorf("suspended sign-in = %d (session cookie set: %v), want 403 and no session",
			cb.StatusCode, cookieValue(t, again, srv.URL, sessionCookieName) != "")
	}
	// The operator and a bystander are untouched.
	if whoamiActor(t, srv, bystander) != "bystander@example.com" {
		t.Error("suspending U signed a bystander out")
	}
	r = do(t, http.MethodGet, srv.URL+"/v1/whoami", byToken, nil, "")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("bystander's PAT = %d, want 200", r.StatusCode)
	}

	resp = suspend(t, srv, op, victimID, false, "appeal upheld")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reinstate = %d, want 200", resp.StatusCode)
	}
	// Reinstatement restores nothing: the old token stays revoked.
	r = do(t, http.MethodGet, srv.URL+"/v1/whoami", token, nil, "")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked PAT after reinstatement = %d, want 401", r.StatusCode)
	}
	back := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	_, settings := statusOf(t, back, srv.URL+"/settings")
	for _, want := range []string{"Operator actions on your account", "posting spam", "appeal upheld", "Suspended", "Reinstated"} {
		if !strings.Contains(settings, want) {
			t.Errorf("the reinstated user's Settings does not show %q", want)
		}
	}
	if strings.Contains(settings, opEmail) {
		t.Error("the audit row shows the operator's email; it must show their handle")
	}
	_, bySettings := statusOf(t, bystander, srv.URL+"/settings")
	if strings.Contains(bySettings, "posting spam") {
		t.Error("a bystander reads the audit row about someone else")
	}
	_, console := statusOf(t, op, srv.URL+"/operator")
	if !strings.Contains(console, "posting spam") || !strings.Contains(console, "appeal upheld") {
		t.Error("the operator console does not list the audit rows")
	}
}

// The console's no-script form: same-origin by the double-submit field,
// answered with a redirect carrying a fixed outcome code, and it refuses to
// let an operator suspend themselves.
func TestIntegrationOperatorConsoleForm(t *testing.T) {
	idp := newFakeIdP(t)
	srv := operatorServer(t, idp, idp.srv.URL+"|"+opSubject, "")
	op := signInAs(t, srv, idp, opSubject, opEmail, nil)
	victim := signInAs(t, srv, idp, victimSubject, victimEmail, nil)
	dir, _ := operatorDirectory(t, srv, op)
	victimID, opID := directoryUserID(t, dir, victimEmail), directoryUserID(t, dir, opEmail)

	post := func(form url.Values) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/operator/suspensions", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := op.Do(req)
		if err != nil {
			t.Fatalf("POST /operator/suspensions: %v", err)
		}
		resp.Body.Close()
		return resp
	}
	csrf := cookieValue(t, op, srv.URL, csrfCookieName)

	if resp := post(url.Values{"user_id": {victimID}, "action": {"suspend"}, "reason": {"r"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form without csrf_token = %d, want 403", resp.StatusCode)
	}
	if resp := post(url.Values{"csrf_token": {csrf}, "user_id": {opID}, "action": {"suspend"}, "reason": {"r"}}); resp.Header.Get("Location") != "/operator?error=self" {
		t.Fatalf("self-suspension redirect = %q, want /operator?error=self", resp.Header.Get("Location"))
	}
	resp := post(url.Values{"csrf_token": {csrf}, "user_id": {victimID}, "action": {"suspend"}, "reason": {"<script>x</script>"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/operator?done=suspended" {
		t.Fatalf("suspend form = %d → %q, want 303 → /operator?done=suspended", resp.StatusCode, resp.Header.Get("Location"))
	}
	if status, _ := statusOf(t, victim, srv.URL+"/whoami"); status != http.StatusSeeOther {
		t.Errorf("the form's suspension left the session live: GET /whoami = %d", status)
	}
	_, page := statusOf(t, op, srv.URL+"/operator?done=suspended&error=%3Cb%3Eforged")
	if !strings.Contains(page, "User suspended.") {
		t.Error("the console does not render the fixed notice")
	}
	if strings.Contains(page, "<script>x</script>") || strings.Contains(page, "<b>forged") {
		t.Error("the console renders unescaped user text")
	}
}
