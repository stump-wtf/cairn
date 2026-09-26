package httpapi

// Captured Request Redaction, End to End
//
// A capture sent to the open ingress is scanned and masked before it is stored
// (SPEC-0017 RD-4). These tests plant runtime-built tokens in the Authorization
// header, the query and the body of a real ingress request, then grep every
// surface the capture reaches: the ingress response (which must not change),
// the REST reads, the inspector page, the SSE stream, the MCP resource and the
// server log. Credential-shaped fixtures come from plantedToken, assembled at
// run time from split literals.
//
// Governing: ADR-0023, SPEC-0017 RD-4, RD-7, RD-9; ADR-0010, SPEC-0005

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

var (
	sharedScannerOnce sync.Once
	sharedScanner     *redact.Scanner
	sharedScannerErr  error
)

// sharedTestScanner is a real scanner with the production defaults, built
// once per test binary and handed to every test server, so the services under
// test scan exactly as cairnd's do.
func sharedTestScanner(t testing.TB) *redact.Scanner {
	t.Helper()
	sharedScannerOnce.Do(func() { sharedScanner, sharedScannerErr = redact.New(redact.Config{}) })
	if sharedScannerErr != nil {
		t.Fatalf("redact.New: %v", sharedScannerErr)
	}
	return sharedScanner
}

// hookRedactionServer is testServer with the scanner chosen and the log in
// reach.
func hookRedactionServer(t *testing.T, sc *redact.Scanner) (*httptest.Server, *syncBuffer) {
	t.Helper()
	cfg := noRateLimit()
	cfg.DevInsecureBearerAuth = true
	cfg.Redaction = sc
	logs := &syncBuffer{}
	st := store.New(newTestPool(t), objectstore.NewMemory(), storeOpts())
	srv := httptest.NewServer(New(st, nil, nil, cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))).Handler())
	t.Cleanup(srv.Close)
	return srv, logs
}

// ingressReply is what a sender sees.
type ingressReply struct {
	status      int
	contentType string
	body        string
}

func sendCapture(t *testing.T, url, body string, headers map[string]string) ingressReply {
	t.Helper()
	resp := hookIngressDo(t, http.MethodPost, url, strings.NewReader(body), headers)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return ingressReply{resp.StatusCode, resp.Header.Get("Content-Type"), string(b)}
}

// getRaw fetches url with the owner's bearer and returns the raw body.
func getRaw(t *testing.T, url string) string {
	t.Helper()
	resp := do(t, http.MethodGet, url, "joe", nil, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	return string(b)
}

// secretCapture is a request carrying three planted tokens: a bearer
// Authorization header, a query parameter and a JSON body field.
type secretCapture struct {
	auth, query, body string
}

func newSecretCapture(seed int) secretCapture {
	return secretCapture{plantedToken(seed), plantedToken(seed + 1), plantedToken(seed + 2)}
}

func (c secretCapture) tokens() []string { return []string{c.auth, c.query, c.body} }

func (c secretCapture) send(t *testing.T, addr string) ingressReply {
	t.Helper()
	return sendCapture(t, addr+"?sig="+c.query, `{"deploy_key":"`+c.body+`"}`, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + c.auth,
	})
}

// assertMaskedView checks one captured request's JSON view: every token is
// gone, Authorization keeps its name and scheme, and the outcome is reported.
func assertMaskedView(t *testing.T, where string, v requestView, c secretCapture) {
	t.Helper()
	assertNoToken(t, where, v.Query+string(v.Body), c.tokens())
	for _, vals := range v.Headers {
		assertNoToken(t, where, strings.Join(vals, "\n"), c.tokens())
	}
	if got := v.Headers["authorization"]; len(got) != 1 || got[0] != "Bearer "+redact.Mask {
		t.Fatalf("%s: authorization = %q, want [Bearer [REDACTED]]", where, got)
	}
	if !strings.Contains(string(v.Body), `"deploy_key":"`+redact.Mask+`"`) || !strings.Contains(v.Query, redact.Mask) {
		t.Fatalf("%s: body %q / query %q not masked in place", where, v.Body, v.Query)
	}
	if v.RedactionStatus != string(redact.StatusMasked) || !v.Redacted || v.Redactions == nil || v.Redactions.Count < 3 {
		t.Fatalf("%s: outcome = %q redacted=%v %+v, want masked with at least 3 values", where, v.RedactionStatus, v.Redacted, v.Redactions)
	}
}

// TestIntegrationHookIngressMasksCaptureAndKeepsResponse is SPEC-0017 RD-4
// "Webhook capture is masked" end to end: the sender's reply is byte for byte
// the reply to a clean capture, and the REST reads, the inspector page and the
// log carry only the masked form.
func TestIntegrationHookIngressMasksCaptureAndKeepsResponse(t *testing.T) {
	srv, logs := hookRedactionServer(t, sharedTestScanner(t))
	created := createHookEndpoint(t, srv.URL)
	addr := ingressAddr(srv.URL, created.ID)

	clean := sendCapture(t, addr+"?p=clean", `{"ok":true}`, map[string]string{"Content-Type": "application/json"})
	c := newSecretCapture(21)
	if got := c.send(t, addr); got != clean {
		t.Fatalf("ingress reply to a capture holding credentials = %+v, want the same reply as a clean one %+v", got, clean)
	}
	if clean.status != http.StatusOK || clean.body != hookIngressAck {
		t.Fatalf("clean reply = %+v, want 200 and the fixed ack", clean)
	}

	raw := getRaw(t, srv.URL+"/v1/hooks/"+created.ID)
	assertNoToken(t, "GET /v1/hooks/{id}", raw, c.tokens())
	var hook hookResponse
	if err := json.Unmarshal([]byte(raw), &hook); err != nil {
		t.Fatal(err)
	}
	if len(hook.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(hook.Requests))
	}
	assertMaskedView(t, "GET /v1/hooks/{id}", hook.Requests[0], c)
	if v := hook.Requests[1]; v.RedactionStatus != string(redact.StatusClean) || v.Redacted {
		t.Fatalf("clean capture outcome = %q redacted=%v, want clean", v.RedactionStatus, v.Redacted)
	}

	raw = getRaw(t, srv.URL+"/v1/hooks/"+created.ID+"/requests/2")
	assertNoToken(t, "GET /v1/hooks/{id}/requests/{seq}", raw, c.tokens())
	var one requestView
	if err := json.Unmarshal([]byte(raw), &one); err != nil {
		t.Fatal(err)
	}
	assertMaskedView(t, "GET /v1/hooks/{id}/requests/{seq}", one, c)

	// The inspector shows the stored, masked values.
	status, html := getHTML(t, srv.URL+"/"+created.ID)
	if status != http.StatusOK {
		t.Fatalf("inspector = %d", status)
	}
	assertNoToken(t, "the inspector page", html, c.tokens())
	if !strings.Contains(html, "Bearer "+redact.Mask) || !strings.Contains(html, "sig="+redact.Mask) {
		t.Fatal("the inspector does not show the masked Authorization header and query as stored")
	}

	assertNoToken(t, "the server log", logs.String(), c.tokens())
}

// TestIntegrationHookStreamCarriesMaskedFormOnly: the live SSE fan-out of a
// capture is the masked form, never what the sender sent.
func TestIntegrationHookStreamCarriesMaskedFormOnly(t *testing.T) {
	srv, _ := hookRedactionServer(t, sharedTestScanner(t))
	created := createHookEndpoint(t, srv.URL)
	sc := openHookStream(t, srv.URL, created.ID, -1)

	c := newSecretCapture(31)
	if got := c.send(t, ingressAddr(srv.URL, created.ID)); got.status != http.StatusOK || got.body != hookIngressAck {
		t.Fatalf("ingress reply = %+v", got)
	}
	ev := sc.next(t)
	assertNoToken(t, "the SSE event", ev.Data, c.tokens())
	assertMaskedView(t, "the SSE event", hookRequestData(t, ev), c)
}

// TestIntegrationMCPHookResourceCarriesMaskedFormOnly: an agent tailing the
// endpoint over MCP is notified of the capture and reads only the masked form.
func TestIntegrationMCPHookResourceCarriesMaskedFormOnly(t *testing.T) {
	srv, _ := mcpTestServer(t, mcpConfig(), store.Options{})
	humanToken := mintMCPToken(t, srv, "sam@stump.rocks", []string{"artifacts:read", "artifacts:write"})
	created := createHookViaMCP(t, srv, humanToken)

	updates := make(chan string, 4)
	sess := mcpClient(t, srv, humanToken, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updates <- req.Params.URI
		},
	}, "hook-redaction-agent")
	uri := "mcp://cairn/hook/" + created.ID
	if err := sess.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	c := newSecretCapture(41)
	if got := c.send(t, ingressAddr(srv.URL, created.ID)); got.status != http.StatusOK || got.body != hookIngressAck {
		t.Fatalf("ingress reply = %+v", got)
	}
	select {
	case <-updates:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a resources/updated notification")
	}

	read, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read hook resource: %v", err)
	}
	text := read.Contents[0].Text
	assertNoToken(t, "the MCP hook resource", text, c.tokens())
	var hook hookResponse
	if err := json.Unmarshal([]byte(text), &hook); err != nil {
		t.Fatal(err)
	}
	if len(hook.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(hook.Requests))
	}
	assertMaskedView(t, "the MCP hook resource", hook.Requests[0], c)
}

// TestIntegrationHookIngressOversizeWithheldNotRefused (RD-7 with
// store-and-flag semantics): a body over the scan cap is not refused to the
// sender. Under the default policy it is withheld, a notice stands in for it,
// and the capture says so. The cap is above the size of the headers, so only
// the body is over it.
func TestIntegrationHookIngressOversizeWithheldNotRefused(t *testing.T) {
	sc, err := redact.New(redact.Config{MaxScanBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	srv, logs := hookRedactionServer(t, sc)
	created := createHookEndpoint(t, srv.URL)
	addr := ingressAddr(srv.URL, created.ID)

	tok := plantedToken(51)
	clean := sendCapture(t, addr, "short", map[string]string{"Content-Type": "text/plain"})
	big := sendCapture(t, addr, strings.Repeat("padding ", 200)+tok, map[string]string{"Content-Type": "text/plain"})
	if big != clean {
		t.Fatalf("ingress reply to an oversize capture = %+v, want the clean reply %+v", big, clean)
	}

	raw := getRaw(t, srv.URL+"/v1/hooks/"+created.ID+"/requests/2")
	assertNoToken(t, "GET /v1/hooks/{id}/requests/{seq}", raw, []string{tok})
	var v requestView
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(v.Body), redact.Mask+" Cairn withheld") || len(v.Withheld) != 1 || v.Withheld[0] != "body" {
		t.Fatalf("body %q withheld %v, want the notice and [body]", v.Body, v.Withheld)
	}
	if v.RedactionStatus != string(redact.StatusNotScannedOversize) {
		t.Fatalf("status = %q, want not_scanned_oversize", v.RedactionStatus)
	}
	line := logs.String()
	assertNoToken(t, "the server log", line, []string{tok})
	if !strings.Contains(line, "captured field withheld") || !strings.Contains(line, "reason=too_large_to_scan") {
		t.Fatalf("log does not name the withheld field and reason:\n%s", line)
	}
}
