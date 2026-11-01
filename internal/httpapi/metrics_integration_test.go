package httpapi

// Metrics Endpoint Credential And Cardinality Tests
//
// GET /metrics is the one endpoint whose reader is infrastructure rather than a
// person, so its credential is the operator's static token and nothing else: an
// artifact-scoped credential reaches one artifact's contents, and must not also
// reach the aggregate counts of every artifact on the instance (SPEC-0014
// REQ-1). The dev bearer is checked separately because it is the interesting
// rejection: it authenticates as the very user the static token names, and is
// still not a credential.
//
// The second test is REQ-5 read off a live scrape after the handle travelled
// through a request path: it appears nowhere in the exposition.
//
// Governing: ADR-0021, ADR-0029 section 1; SPEC-0014 REQ-1, REQ-3, REQ-5
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/oauth"
	"github.com/stump-wtf/cairn/internal/pat"
	"github.com/stump-wtf/cairn/internal/sharetype"
	"github.com/stump-wtf/cairn/internal/store"
	"github.com/stump-wtf/cairn/internal/user"
)

// scrapeTokenSecret is the CAIRN_API_TOKENS entry standing in for a deployment's
// Prometheus credential; it names alice, whom withTokenOperators makes an
// operator, as a human token does.
const scrapeTokenSecret = "sk_metrics_scrape_0f1e2d3c4b5a69788796a5b4c3d2e1f0"

// metricsServer stands up the adapter exactly as a scraped cairn serve boots:
// the real OAuth AS, an operator, one static token naming her, and the
// scrape-time storage collector registered over the store it serves.
func metricsServer(t *testing.T, reg *metrics.Registry) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := mcpConfig()
	cfg.Metrics = reg
	cfg.APITokens = []APIToken{{Secret: scrapeTokenSecret, User: "alice"}}
	opts := withScanner(store.Options{MaxUploadBytes: 1 << 20, Metrics: reg})
	srv, st := mcpTestServer(t, cfg, opts)
	reg.RegisterStorage(st, sharetype.Default().KeyedTypes())
	return srv, st
}

// scrapeMetrics GETs /metrics with token and returns status and body.
func scrapeMetrics(t *testing.T, url, token string) (int, string) {
	t.Helper()
	resp := do(t, http.MethodGet, url+"/metrics", token, nil, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// TestIntegrationMetricsEndpointCredential walks the credential shapes SPEC-0014
// REQ-1 names: nothing at all, an artifact-scoped token, and the operator's
// static token. A rejected request answers 401 with an empty body, so a prober
// cannot learn whether the endpoint is populated.
func TestIntegrationMetricsEndpointCredential(t *testing.T) {
	reg := metrics.New()
	srv, st := metricsServer(t, reg)

	alice, err := user.NewStore(st.Pool()).ResolveActor(context.Background(), "alice")
	if err != nil {
		t.Fatalf("resolve alice: %v", err)
	}
	patSvc := pat.NewService(st.Pool())
	scopes := []string{oauth.ScopeArtifactsRead, oauth.ScopeArtifactsWrite}
	humanPAT, _, err := patSvc.Create(context.Background(), alice.ID, "cli", scopes, false)
	if err != nil {
		t.Fatalf("create PAT: %v", err)
	}
	mcpToken := mintMCPToken(t, srv, "alice", scopes)

	for _, tc := range []struct {
		name     string
		token    string
		want     int
		bodyMust bool
	}{
		{"no credential", "", http.StatusUnauthorized, false},
		{"a human PAT, artifact-scoped", humanPAT, http.StatusUnauthorized, false},
		{"an MCP OAuth access token", mcpToken, http.StatusUnauthorized, false},
		{"the operator's static token", scrapeTokenSecret, http.StatusOK, true},
	} {
		status, body := scrapeMetrics(t, srv.URL, tc.token)
		if status != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, status, tc.want)
		}
		if !tc.bodyMust && body != "" {
			t.Errorf("%s: rejected request leaked a body: %q", tc.name, body)
		}
		if tc.bodyMust && !strings.Contains(body, "# TYPE cairn_artifacts_total gauge") {
			t.Errorf("%s: exposition missing the storage gauge:\n%s", tc.name, body)
		}
	}
}

// TestIntegrationMetricsDevBearerIsNotACredential is the rejection that a
// credential test without it would miss: with the dev bearer shortcut enabled,
// "Bearer alice" authenticates as alice, the same user the static token names,
// so a check phrased as "is this principal an operator's user" would wave the
// scraper through. The presented secret is the credential; a user id is not.
func TestIntegrationMetricsDevBearerIsNotACredential(t *testing.T) {
	reg := metrics.New()
	cfg := mcpConfig()
	cfg.Metrics = reg
	cfg.DevInsecureBearerAuth = true
	cfg.APITokens = []APIToken{{Secret: scrapeTokenSecret, User: "alice"}}
	srv := testServer(t, cfg, withScanner(store.Options{MaxUploadBytes: 1 << 20}))

	if status, body := scrapeMetrics(t, srv.URL, "alice"); status != http.StatusUnauthorized || body != "" {
		t.Errorf("dev bearer acting as the operator's own user: status = %d body = %q, want 401 empty", status, body)
	}
	if status, _ := scrapeMetrics(t, srv.URL, scrapeTokenSecret); status != http.StatusOK {
		t.Errorf("static operator token: status = %d, want 200", status)
	}
}

// TestIntegrationMetricsLabelsCarryNoArtifactData is SPEC-0014 REQ-5 at the seam
// where it matters: an artifact is created and then read by handle, so its
// public id crosses a request path, and every one of those requests is counted.
// The exposition may describe the traffic; it may not name the traffic's
// subject, because the endpoint's reader is a scraper that reaches every
// instance's metrics.
func TestIntegrationMetricsLabelsCarryNoArtifactData(t *testing.T) {
	reg := metrics.New()
	srv, _ := metricsServer(t, reg)

	id := createArtifact(t, srv.URL, "file", scrapeTokenSecret, "a handoff's worth of text")
	if resp := do(t, http.MethodGet, srv.URL+"/v1/artifacts/"+id, scrapeTokenSecret, nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("artifact read: status = %d", resp.StatusCode)
	}
	resp := do(t, http.MethodGet, srv.URL+"/v1/whoami", scrapeTokenSecret, nil, "")
	resp.Body.Close()

	_, body := scrapeMetrics(t, srv.URL, scrapeTokenSecret)
	for _, leak := range []string{id, "alice", `handle="`, `actor="`, `path="`} {
		if strings.Contains(body, leak) {
			t.Errorf("%q reached the exposition:\n%s", leak, body)
		}
	}

	classes := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "cairn_http_requests_total{") {
			continue
		}
		_, rest, ok := strings.Cut(line, `route_class="`)
		if !ok {
			t.Errorf("request series without a route_class: %s", line)
			continue
		}
		value, _, _ := strings.Cut(rest, `"`)
		classes[value] = true
	}
	for class := range classes {
		if !slicesContains(metrics.HTTPRouteClasses, class) {
			t.Errorf("route_class %q is outside the closed set", class)
		}
	}
	if !classes[string(metrics.ClassArtifactRead)] {
		t.Errorf("artifact reads never counted; route_class set = %v", classes)
	}
	if !classes[string(metrics.ClassOther)] {
		t.Errorf("nothing classified as other; route_class set = %v", classes)
	}
}

// TestRouteClassForUnmappedPatternIsOther is the classifier's own control: a
// route the map does not name is counted as other rather than dropped, which is
// how a stale map becomes visible instead of silently under-counting.
func TestRouteClassForUnmappedPatternIsOther(t *testing.T) {
	for _, pattern := range []string{"", "/v1/something-new", "/brand/new", "/h/{unknown}"} {
		if got := routeClassFor(pattern); got != metrics.ClassOther {
			t.Errorf("routeClassFor(%q) = %q, want other", pattern, got)
		}
	}
}

// slicesContains is slices.Contains over a slice of string-typed constants,
// which generics would otherwise need a type parameter for at each call.
func slicesContains(set []metrics.HTTPRouteClass, want string) bool {
	for _, c := range set {
		if string(c) == want {
			return true
		}
	}
	return false
}
