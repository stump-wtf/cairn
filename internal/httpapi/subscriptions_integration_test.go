package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/store"
)

// Governing: SPEC-0023 REQ "Owned Outbound Subscriptions", REQ "Removing the
// Instance-Wide Outbound Targets" (scenario "The operator replaces their
// firehose"), REQ "Subscription Target Safety"

const (
	subsOpToken    = "sk_subs_operator_human_4c1e7a9b3d5f"
	subsAgentToken = "sk_subs_operator_agent_8e2a6c0f4b1d"
	subsOperator   = "op@example.com"
)

// switchboardSecret stands in for a Switchboard cairn webhook's
// signing_secret: receiver-issued, at least 32 bytes. Built rather than
// written as one literal so the secret scanner does not mistake a test
// fixture for a credential.
var switchboardSecret = "sb_whsec_" + strings.Repeat("receiver-issued-", 3)

// subsAPIServer is the adapter with a dev login (sessions), the dev bearer
// (artifact creation as any actor), and two static tokens naming the
// operator: human and agent. withKey false leaves CAIRN_ENCRYPTION_KEY unset.
// logs, when non-nil, receives the server's log.
func subsAPIServer(t *testing.T, withKey bool, logs io.Writer) (*httptest.Server, *testSubs) {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	tokens, err := ParseAPITokens(subsOpToken + ":" + subsOperator + ":human," + subsAgentToken + ":" + subsOperator + ":agent")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BaseURL:               "http://cairn.test",
		MaxUploadBytes:        1 << 20,
		DefaultTTL:            time.Hour,
		DevLoginPassword:      "devpass",
		SessionTTL:            time.Hour,
		DevInsecureBearerAuth: true,
		APITokens:             tokens,
		RatePerSecond:         1000,
		RateBurst:             1000,
	}
	pool := newTestPool(t)
	withTokenOperators(t, pool, &cfg)
	cfg.Subscriptions = newTestSubscriptions(pool, testSubsPolicy(), withKey)
	var opts store.Options
	wireSubscriptions(t, pool, &cfg, &opts)
	st := store.New(pool, objectstore.NewMemory(), opts)
	srv := httptest.NewServer(newResolvedServer(t, st, cfg, slog.New(slog.NewTextHandler(logs, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv, &testSubs{Service: cfg.Subscriptions, pool: pool}
}

// sessionFor signs actor in with the dev login and returns its client.
func sessionFor(t *testing.T, srv *httptest.Server, actor string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp := doLogin(t, srv, client, actor, "devpass")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s = %d", actor, resp.StatusCode)
	}
	return client
}

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// bearerJSON issues a JSON request with a bearer token.
func bearerJSON(t *testing.T, method, target, token string, v any) *http.Response {
	t.Helper()
	var body io.Reader
	if v != nil {
		body = jsonBody(t, v)
	}
	return do(t, method, target, token, body, "application/json")
}

type subscriptionCreated struct {
	subscriptionView
	Secret string `json:"secret"`
}

func decodeSub(t *testing.T, resp *http.Response, want int) subscriptionCreated {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, b)
	}
	var out subscriptionCreated
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return out
}

func listSubs(t *testing.T, srvURL string, client *http.Client) (listSubscriptionsResponse, string) {
	t.Helper()
	resp := sessionJSON(t, srvURL, client, http.MethodGet, "/v1/subscriptions", nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d: %s", resp.StatusCode, b)
	}
	var out listSubscriptionsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out, string(b)
}

// createTagged creates an artifact tagged handoff as the bearer actor.
func createTagged(t *testing.T, srvURL, bearer string) string {
	t.Helper()
	resp := postTagged(t, srvURL+"/v1/artifacts?type=markdown&tag=handoff", bearer, strings.NewReader("# handoff"), "text/markdown")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, b)
	}
	return decodeArtifact(t, resp).ID
}

func eventID(t *testing.T, ev hookedEvent) string {
	t.Helper()
	var body struct {
		Data struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(ev.body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Data.ID
}

// TestIntegrationUserWiresOwnSwitchboard is scenario "A user wires their own
// Switchboard": U creates a subscription over /v1, is shown a minted secret
// once, creates an artifact, and one signed artifact.created delivery reaches
// the URL, verifiable with that secret.
func TestIntegrationUserWiresOwnSwitchboard(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")

	created := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL, "event_types": []string{"artifact.created"}, "tags": []string{"handoff"}})),
		http.StatusCreated)
	if !strings.HasPrefix(created.Secret, "whsec_") || !created.Active || created.Owner == "" {
		t.Fatalf("created = %+v", created)
	}

	list, raw := listSubs(t, srv.URL, u)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0].ID != created.ID || list.Limit != 5 || !list.Available {
		t.Fatalf("list = %+v", list)
	}
	if strings.Contains(raw, created.Secret) {
		t.Fatal("the list shows the secret again")
	}
	resp := sessionJSON(t, srv.URL, u, http.MethodGet, "/v1/subscriptions/"+created.ID, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), created.Secret) {
		t.Fatalf("GET one = %d, shows secret: %v", resp.StatusCode, strings.Contains(string(b), created.Secret))
	}

	id := createTagged(t, srv.URL, "u@example.com")
	ev := recv.wait(t)
	if eventID(t, ev) != id {
		t.Fatalf("delivered %s, want %s", eventID(t, ev), id)
	}
	verifySignature(t, ev, created.Secret)
	time.Sleep(150 * time.Millisecond)
	if recv.count() != 1 {
		t.Fatalf("%d deliveries, want exactly one", recv.count())
	}
}

// TestIntegrationReceiverIssuedSecret is scenario "Receiver-issued secret":
// deliveries verify under the secret the receiver issued, unchanged.
func TestIntegrationReceiverIssuedSecret(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")
	created := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL, "secret": switchboardSecret})), http.StatusCreated)
	if created.Secret != switchboardSecret {
		t.Fatalf("secret = %q, want the supplied one back", created.Secret)
	}
	createTagged(t, srv.URL, "u@example.com")
	verifySignature(t, recv.wait(t), switchboardSecret)

	// A supplied secret shorter than 32 bytes is refused, and not echoed.
	resp := sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL, "secret": "short-secret"}))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(string(b), "short-secret") {
		t.Fatalf("short secret = %d %s", resp.StatusCode, b)
	}
}

// TestIntegrationTeamSubscriptionRefused is scenario "Member tries to add a
// team subscription": until teams exist no caller administers one, so every
// team-owned request is 403 and creates nothing. Naming another user as the
// owner is refused the same way.
func TestIntegrationTeamSubscriptionRefused(t *testing.T) {
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")
	for _, owner := range []string{"team:stump", "team:", "user:11111111-1111-4111-8111-111111111111", "someone"} {
		resp := sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
			jsonBody(t, map[string]any{"owner": owner, "url": "https://hooks.example.com/w"}))
		env := decodeError(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("owner %q = %d, want 403", owner, resp.StatusCode)
		}
		if strings.HasPrefix(owner, "team:") && env.Error.Details["reason"] != "team_role_required" {
			t.Fatalf("owner %q details = %v", owner, env.Error.Details)
		}
	}
	if list, _ := listSubs(t, srv.URL, u); len(list.Subscriptions) != 0 {
		t.Fatalf("refused requests created %d subscriptions", len(list.Subscriptions))
	}
	resp := sessionJSON(t, srv.URL, u, http.MethodGet, "/v1/subscriptions?owner=team:stump", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("listing a team's subscriptions = %d, want 403", resp.StatusCode)
	}
}

// TestIntegrationSubscriptionsAreOwnerScoped: a second user gets nothing —
// not in a list, and a uniform 404 for every call naming U's subscription.
func TestIntegrationSubscriptionsAreOwnerScoped(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")
	v := sessionFor(t, srv, "v@example.com")
	created := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL})), http.StatusCreated)

	if list, raw := listSubs(t, srv.URL, v); len(list.Subscriptions) != 0 || strings.Contains(raw, recv.srv.URL) {
		t.Fatalf("V's list shows U's subscription: %s", raw)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/subscriptions/" + created.ID, nil},
		{http.MethodPatch, "/v1/subscriptions/" + created.ID, map[string]any{"paused": true}},
		{http.MethodPost, "/v1/subscriptions/" + created.ID + "/rotate", map[string]any{}},
		{http.MethodDelete, "/v1/subscriptions/" + created.ID, nil},
	} {
		var body io.Reader
		if c.body != nil {
			body = jsonBody(t, c.body)
		}
		resp := sessionJSON(t, srv.URL, v, c.method, c.path, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("V %s %s = %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
	if list, _ := listSubs(t, srv.URL, u); len(list.Subscriptions) != 1 || !list.Subscriptions[0].Active {
		t.Fatalf("V's calls changed U's subscription: %+v", list)
	}
	// V's artifacts never reach U's subscription.
	createTagged(t, srv.URL, "v@example.com")
	time.Sleep(200 * time.Millisecond)
	if recv.count() != 0 {
		t.Fatal("V's artifact reached U's subscription")
	}
}

// TestIntegrationSubscriptionsNeedAHuman: an agent credential can never point
// its human's events somewhere new — the dev bearer (agent scopes), an agent
// static token and a PAT are refused — while the operator's human static
// token can script it, and a browser session needs its CSRF header.
func TestIntegrationSubscriptionsNeedAHuman(t *testing.T) {
	srv, _ := subsAPIServer(t, true, nil)
	body := map[string]any{"url": "https://hooks.example.com/w"}
	for name, token := range map[string]string{"dev bearer": "u@example.com", "agent static token": subsAgentToken} {
		resp := bearerJSON(t, http.MethodPost, srv.URL+"/v1/subscriptions", token, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s create = %d, want 403", name, resp.StatusCode)
		}
		resp = bearerJSON(t, http.MethodGet, srv.URL+"/v1/subscriptions", token, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s list = %d, want 403", name, resp.StatusCode)
		}
	}

	u := sessionFor(t, srv, "u@example.com")
	pat := createPAT(t, srv, u, "script", []string{scopeArtifactsRead, scopeArtifactsWrite, scopeAnnotationsWrite}, false)
	resp := bearerJSON(t, http.MethodPost, srv.URL+"/v1/subscriptions", pat.Token, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PAT create = %d, want 403", resp.StatusCode)
	}

	// A session without the CSRF header is refused.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/subscriptions", jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("session create without CSRF = %d, want 403", resp.StatusCode)
	}
	resp = bearerJSON(t, http.MethodGet, srv.URL+"/v1/subscriptions", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d, want 401", resp.StatusCode)
	}
}

// TestIntegrationOperatorReplacesFirehose is scenario "The operator replaces
// their firehose": the operator creates a subscription they own, supplying
// their Switchboard webhook's signing secret; their artifacts reach it signed
// as before, and no other user's artifact does. Neither secret appears in
// the server log.
func TestIntegrationOperatorReplacesFirehose(t *testing.T) {
	recv := newHookReceiver(t)
	logs := &syncBuffer{}
	srv, _ := subsAPIServer(t, true, logs)
	created := decodeSub(t, bearerJSON(t, http.MethodPost, srv.URL+"/v1/subscriptions", subsOpToken,
		map[string]any{"url": recv.srv.URL, "secret": switchboardSecret, "tags": []string{"handoff"}}), http.StatusCreated)
	if created.Secret != switchboardSecret {
		t.Fatal("the supplied secret did not come back")
	}

	other := createTagged(t, srv.URL, "dave@example.com")
	mine := createTagged(t, srv.URL, subsOpToken)
	ev := recv.wait(t)
	if got := eventID(t, ev); got != mine {
		t.Fatalf("the operator's subscription received %s, want the operator's %s (not %s)", got, mine, other)
	}
	verifySignature(t, ev, switchboardSecret)
	time.Sleep(200 * time.Millisecond)
	if recv.count() != 1 {
		t.Fatalf("%d deliveries, want only the operator's artifact", recv.count())
	}
	for _, secret := range []string{switchboardSecret, subsOpToken, strings.TrimPrefix(recv.srv.URL, "http://")} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the server log contains %q", secret)
		}
	}
}

// TestIntegrationSubscriptionLifecycleOverV1: pause stops deliveries, resume
// restarts them, rotate replaces the secret (the old one no longer
// verifies), delete removes it; the ceiling and a refused target answer with
// a reason that never quotes the URL.
func TestIntegrationSubscriptionLifecycleOverV1(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")
	created := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL})), http.StatusCreated)
	path := "/v1/subscriptions/" + created.ID

	paused := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPatch, path, jsonBody(t, map[string]any{"paused": true})), http.StatusOK)
	if paused.Active || !paused.Paused {
		t.Fatalf("pause = %+v", paused)
	}
	createTagged(t, srv.URL, "u@example.com")
	time.Sleep(200 * time.Millisecond)
	if recv.count() != 0 {
		t.Fatal("a paused subscription received a delivery")
	}

	decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPatch, path, jsonBody(t, map[string]any{"paused": false})), http.StatusOK)
	rotated := decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, path+"/rotate", nil), http.StatusOK)
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatal("rotate did not return a new secret")
	}
	createTagged(t, srv.URL, "u@example.com")
	ev := recv.wait(t)
	verifySignature(t, ev, rotated.Secret)

	resp := sessionJSON(t, srv.URL, u, http.MethodDelete, path, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	// A private target is refused at creation, with a reason and no URL.
	const private = "https://10.9.8.7/webhooks/w/capability-token"
	resp = sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions", jsonBody(t, map[string]any{"url": private}))
	env := decodeError(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(env.Error.Details["reason"], "non-public") ||
		strings.Contains(env.Error.Details["reason"], "capability-token") {
		t.Fatalf("private target = %d %+v", resp.StatusCode, env.Error)
	}
	resp = sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions",
		jsonBody(t, map[string]any{"url": recv.srv.URL, "event_types": []string{"artifact.exploded"}}))
	if env := decodeError(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(env.Error.Details["reason"], "artifact.exploded") {
		t.Fatalf("unknown event type = %d %+v", resp.StatusCode, env.Error)
	}

	// The per-user ceiling is 5.
	for i := 0; i < 5; i++ {
		decodeSub(t, sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions", jsonBody(t, map[string]any{"url": recv.srv.URL})), http.StatusCreated)
	}
	resp = sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions", jsonBody(t, map[string]any{"url": recv.srv.URL}))
	if env := decodeError(t, resp); resp.StatusCode != http.StatusConflict ||
		env.Error.Details["reason"] != "subscription_ceiling_reached" || env.Error.Details["limit"] != "5" {
		t.Fatalf("sixth subscription = %d %+v", resp.StatusCode, env.Error)
	}
}

// TestIntegrationSubscriptionsWithoutKey: with no CAIRN_ENCRYPTION_KEY the
// server runs, lists say subscriptions are unavailable, and create answers
// 409 encryption_key_unset rather than storing a plaintext secret.
func TestIntegrationSubscriptionsWithoutKey(t *testing.T) {
	srv, _ := subsAPIServer(t, false, nil)
	u := sessionFor(t, srv, "u@example.com")
	if list, _ := listSubs(t, srv.URL, u); list.Available {
		t.Fatal("list says available without a key")
	}
	resp := sessionJSON(t, srv.URL, u, http.MethodPost, "/v1/subscriptions", jsonBody(t, map[string]any{"url": "https://hooks.example.com/w"}))
	if env := decodeError(t, resp); resp.StatusCode != http.StatusConflict || env.Error.Details["reason"] != "encryption_key_unset" {
		t.Fatalf("create without a key = %d %+v", resp.StatusCode, env.Error)
	}
	status, html := getHTMLClient(t, u, srv.URL+"/settings")
	if status != http.StatusOK || !strings.Contains(html, "data-subscription-unavailable") || strings.Contains(html, "data-subscription-form") {
		t.Fatalf("Settings without a key = %d; unavailable notice shown: %v", status, strings.Contains(html, "data-subscription-unavailable"))
	}
}

// settingsForm posts the Settings subscription form as client.
func settingsForm(t *testing.T, srv *httptest.Server, client *http.Client, form url.Values) (int, string) {
	t.Helper()
	form.Set("csrf_token", cookieValue(t, client, srv.URL, csrfCookieName))
	resp := postForm(t, client, srv.URL+"/settings/subscriptions", form, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestIntegrationSettingsSubscriptions: the Settings section creates a
// subscription and shows its secret once (never on a later load), lists it
// with its health, pauses, rotates and deletes it — all without script.
func TestIntegrationSettingsSubscriptions(t *testing.T) {
	recv := newHookReceiver(t)
	srv, _ := subsAPIServer(t, true, nil)
	u := sessionFor(t, srv, "u@example.com")

	status, html := getHTMLClient(t, u, srv.URL+"/settings")
	if status != http.StatusOK || !strings.Contains(html, `id="subscriptions"`) || !strings.Contains(html, "data-subscription-empty") {
		t.Fatalf("/settings = %d; section present: %v", status, strings.Contains(html, `id="subscriptions"`))
	}

	status, html = settingsForm(t, srv, u, url.Values{"action": {"create"}, "url": {recv.srv.URL},
		"secret": {switchboardSecret}, "event_types": {"artifact.created"}, "tags": {"handoff, lane:m"}})
	if status != http.StatusOK || !strings.Contains(html, "data-subscription-secret-value>"+switchboardSecret) {
		t.Fatalf("create form = %d; secret shown once: %v", status, strings.Contains(html, switchboardSecret))
	}
	list, _ := listSubs(t, srv.URL, u)
	if len(list.Subscriptions) != 1 || strings.Join(list.Subscriptions[0].Tags, ",") != "handoff,lane:m" {
		t.Fatalf("form created %+v", list.Subscriptions)
	}
	id := list.Subscriptions[0].ID

	createTagged(t, srv.URL, "u@example.com")
	verifySignature(t, recv.wait(t), switchboardSecret)
	waitHealthOver(t, srv.URL, u, id)

	_, html = getHTMLClient(t, u, srv.URL+"/settings")
	if strings.Contains(html, switchboardSecret) {
		t.Fatal("a later Settings load shows the secret again")
	}
	for _, frag := range []string{recv.srv.URL, "events: artifact.created", "tags: handoff, lane:m", "delivered (202)", "active"} {
		if !strings.Contains(html, frag) {
			t.Errorf("/settings missing %q", frag)
		}
	}

	status, _ = settingsForm(t, srv, u, url.Values{"action": {"pause"}, "id": {id}})
	if status != http.StatusSeeOther {
		t.Fatalf("pause form = %d, want 303", status)
	}
	if list, _ = listSubs(t, srv.URL, u); list.Subscriptions[0].Active {
		t.Fatal("the pause form did not pause")
	}
	status, html = settingsForm(t, srv, u, url.Values{"action": {"rotate"}, "id": {id}})
	if status != http.StatusOK || !strings.Contains(html, "data-subscription-secret-value>whsec_") {
		t.Fatalf("rotate form = %d", status)
	}
	status, html = settingsForm(t, srv, u, url.Values{"action": {"create"}, "url": {"https://10.1.1.1/x"}})
	if status != http.StatusOK || !strings.Contains(html, "data-subscription-error") || !strings.Contains(html, "non-public") {
		t.Fatalf("refused create form = %d; error shown: %v", status, strings.Contains(html, "data-subscription-error"))
	}
	status, _ = settingsForm(t, srv, u, url.Values{"action": {"delete"}, "id": {id}})
	if status != http.StatusSeeOther {
		t.Fatalf("delete form = %d, want 303", status)
	}
	if list, _ = listSubs(t, srv.URL, u); len(list.Subscriptions) != 0 {
		t.Fatal("the delete form did not delete")
	}

	// Without the form's CSRF field, or from a bearer credential, the form
	// does nothing.
	resp := postForm(t, u, srv.URL+"/settings/subscriptions", url.Values{"action": {"create"}, "url": {recv.srv.URL}}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form without CSRF = %d, want 403", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/settings/subscriptions",
		strings.NewReader(url.Values{"action": {"create"}, "url": {recv.srv.URL}, "csrf_token": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+subsOpToken)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "x"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form from a bearer token = %d, want 403", resp.StatusCode)
	}
	if list, _ = listSubs(t, srv.URL, u); len(list.Subscriptions) != 0 {
		t.Fatal("a refused form created a subscription")
	}
}

// waitHealthOver waits until /v1 reports a delivery attempt on id.
func waitHealthOver(t *testing.T, srvURL string, client *http.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ := listSubs(t, srvURL, client)
		for _, s := range list.Subscriptions {
			if s.ID == id && s.LastAttemptAt != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no delivery attempt recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
