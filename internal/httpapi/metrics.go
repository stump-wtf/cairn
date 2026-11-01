package httpapi

// HTTP request metrics and the GET /metrics endpoint (SPEC-0014 REQ-1, REQ-3,
// REQ-5; ADR-0021).
//
// Two things live here. The scrape endpoint is authenticated with the operator
// credential and nothing else: Cairn's metrics describe what everyone stores,
// so an artifact-scoped token, the credential that
// reaches one artifact's contents, must not also reach the aggregate counts of all of them. An
// unauthenticated or wrong-credential request answers 401 with no body, so a
// prober cannot learn even whether the endpoint is populated.
//
// The request counter labels routes with a hand-maintained class, never with
// the path: artifact handles ARE path segments, so a derived label would mint
// one time series per artifact and publish every handle to anyone who can read
// the endpoint. The map is keyed by chi's matched route pattern, which contains
// no user data, and anything it does not name counts as "other"; that bucket
// rising is the classifier's own control, without which it would decay silently
// into one bucket.
//
// Governing: ADR-0021, SPEC-0014 REQ-1, REQ-3, REQ-5; ADR-0029 section 1
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stump-wtf/cairn/internal/event"
	"github.com/stump-wtf/cairn/internal/metrics"
)

// apiRouteClasses maps a /v1 route pattern to its class. Every entry is a
// literal written by hand: the pattern's wildcards are chi's own {name}
// placeholders, never a value from the request. A route missing from this map
// counts as "other": adding it here is the fix, and cairn_http_requests_total
// {route_class="other"} is what tells you the map is stale (SPEC-0014 REQ-3).
var apiRouteClasses = map[string]metrics.HTTPRouteClass{
	// Artifact reads: the content a link can already reach.
	"/v1/artifacts/{id}":                 metrics.ClassArtifactRead,
	"/v1/artifacts/{id}/body":            metrics.ClassArtifactRead,
	"/v1/artifacts/{id}/policy":          metrics.ClassArtifactRead,
	"/v1/artifacts/{id}/rotate":          metrics.ClassArtifactRead,
	"/v1/artifacts/{id}/ttl":             metrics.ClassArtifactRead,
	"/v1/artifacts/{id}/members/*":       metrics.ClassBundle,
	"/v1/artifacts/{id}/comments":        metrics.ClassComment,
	"/v1/artifacts/{id}/reactions":       metrics.ClassComment,
	"/v1/artifacts/{id}/reactions/{rid}": metrics.ClassComment,

	// Creates, the surface the handoff path depends on (REQ-4).
	"/v1/artifacts": metrics.ClassArtifactCreate,

	// Trajectory runs.
	"/v1/runs/{id}":                       metrics.ClassRun,
	"/v1/runs/{id}/spans/{spanID}/output": metrics.ClassRun,
	"/v1/runs/{id}/stream":                metrics.ClassRun,

	// Webhook endpoints and their captured requests.
	"/v1/hooks/{id}":                     metrics.ClassWebhook,
	"/v1/hooks/{id}/requests":            metrics.ClassWebhook,
	"/v1/hooks/{id}/requests/{seq}":      metrics.ClassWebhook,
	"/v1/hooks/{id}/requests/{seq}/body": metrics.ClassWebhook,
	"/v1/hooks/{id}/stream":              metrics.ClassWebhook,

	// Everything else under /v1: the instance's own surfaces (Bin, whoami,
	// tokens, PATs, MCP sessions, OAuth grants, subscriptions, the operator
	// console's JSON) and the OAuth protocol endpoints. None of them is a
	// data-plane route worth its own series.
	"/v1":                      metrics.ClassOther,
	"/v1/bin":                  metrics.ClassOther,
	"/v1/whoami":               metrics.ClassOther,
	"/v1/tokens":               metrics.ClassOther,
	"/v1/tokens/{id}":          metrics.ClassOther,
	"/v1/mcp/sessions":         metrics.ClassOther,
	"/v1/mcp/sessions/{id}":    metrics.ClassOther,
	"/v1/oauth/grants":         metrics.ClassOther,
	"/v1/oauth/grants/{id}":    metrics.ClassOther,
	"/v1/subscriptions":        metrics.ClassOther,
	"/v1/subscriptions/":       metrics.ClassOther,
	"/v1/subscriptions/{id}":   metrics.ClassOther,
	"/v1/operator/directory":   metrics.ClassOther,
	"/v1/operator/suspensions": metrics.ClassOther,
	"/oauth/token":             metrics.ClassOther,
	"/oauth/register":          metrics.ClassOther,
	"/oauth/revoke":            metrics.ClassOther,
	"/oauth/authorize":         metrics.ClassOther,
	"/.well-known/oauth-authorization-server":   metrics.ClassOther,
	"/.well-known/oauth-protected-resource":     metrics.ClassOther,
	"/.well-known/oauth-protected-resource/mcp": metrics.ClassOther,
	"/mcp": metrics.ClassOther,
}

// webRouteClasses maps the web app shell's patterns. The artifact shell page is
// a read of one artifact by handle, so it counts with the other reads; the rest
// of the shell is one class.
var webRouteClasses = map[string]metrics.HTTPRouteClass{
	"/":              metrics.ClassWeb,
	"/bin":           metrics.ClassWeb,
	"/whoami":        metrics.ClassWeb,
	"/settings":      metrics.ClassWeb,
	"/operator":      metrics.ClassWeb,
	"/connect":       metrics.ClassWeb,
	"/login":         metrics.ClassWeb,
	"/auth/login":    metrics.ClassWeb,
	"/auth/callback": metrics.ClassWeb,
	"/logout":        metrics.ClassWeb,
	"/assets/*":      metrics.ClassWeb,
	"/{id}":          metrics.ClassArtifactRead,
	"/run/{id}":      metrics.ClassRun,
}

// hookRouteClasses is the open webhook ingress.
var hookRouteClasses = map[string]metrics.HTTPRouteClass{
	"/h/{id}": metrics.ClassWebhook,
}

// routeClassFor resolves chi's matched pattern to a class. An unmatched or
// unrouted request has no pattern and counts as other, which is where a stale
// map becomes visible.
func routeClassFor(pattern string) metrics.HTTPRouteClass {
	switch {
	case pattern == "":
		return metrics.ClassOther
	case strings.HasPrefix(pattern, "/v1"):
		if class, ok := apiRouteClasses[pattern]; ok {
			return class
		}
		return metrics.ClassOther
	case strings.HasPrefix(pattern, "/h/"):
		if class, ok := hookRouteClasses[pattern]; ok {
			return class
		}
		return metrics.ClassOther
	default:
		if class, ok := webRouteClasses[pattern]; ok {
			return class
		}
		return metrics.ClassOther
	}
}

// instrumentRoutes counts the requests that pass through it. It MUST be applied
// below chi's routing (as a Route/Routes/Group middleware, not a router-wide
// Use), which is the only position where chi has already recorded the matched
// pattern. The wrapped ResponseWriter passes Flush through, so the SSE streams
// keep streaming, and Unwrap, so anything reaching for the real writer can.
func (s *Server) instrumentRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg := s.cfg.Metrics
		if reg == nil {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// The class is read AFTER serving: a route can rewrite the pattern
		// (Mount, Route), and the class must describe what actually served.
		pattern := chi.RouteContext(r.Context()).RoutePattern()
		reg.ObserveHTTP(routeClassFor(pattern), r.Method, rec.status, time.Since(start).Seconds())
	})
}

// statusRecorder remembers only the status code. It deliberately implements
// just http.Flusher and http.Unwrap: it is not a ResponseWriter copy, and a
// handler reaching for Hijacker or io.ReaderFrom gets the real one through
// Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// handleMetrics serves the Prometheus text exposition. Access is decided here,
// ahead of any artifact-scoped authorization: the operator credential is the
// only credential that reaches this endpoint.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.isOperatorCredential(r) {
		// 401 with an empty body: share types and counts describe usage
		// patterns, and an unauthenticated prober should learn nothing,
		// including whether the endpoint is populated (SPEC-0014 REQ-1).
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"cairn\"")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if s.cfg.Metrics == nil {
		// Nothing was instrumented (a wiring without Config.Metrics). Answered
		// only after the credential check, so the route's existence stays
		// invisible to an unauthenticated prober.
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	promhttp.HandlerFor(s.cfg.Metrics.Gatherer(), promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	}).ServeHTTP(w, r)
}

// isOperatorCredential reports whether the request carries the operator
// credential: a CAIRN_API_TOKENS entry, which by construction can only name an
// operator's user, because boot refuses any other (SPEC-0023
// REQ "Static API Tokens Act as an Operator's User"), so a resolved one is the instance's operator
// credential and the scrape credential ADR-0021 assumed but never defined.
//
// What it is NOT: a PAT or an OAuth token, which are artifact-scoped and reach
// their owner's artifacts rather than everyone's aggregates; a browser session,
// whose operator status comes from the session's provenance and which no
// scraper can present; or a dev bearer, which is not a credential at all. The
// checks below refuse all of them.
//
// Governing: SPEC-0014 REQ-1, design "Auth"; ADR-0029 section 1, 7.
func (s *Server) isOperatorCredential(r *http.Request) bool {
	p, err := s.auth.Authenticate(r)
	if err != nil || p == nil {
		return false
	}
	if p.Auth != event.AuthAPIToken {
		return false
	}
	// A static token authenticates only after ResolveAPITokens has bound it to
	// a user, and the bearer presented must be that credential itself: the dev
	// bearer shortcut shares the Auth stamp and resolves a user, but is not a
	// credential anyone could hand to a scraper.
	if p.UserID == "" || !s.staticTokens.presents(r) {
		return false
	}
	return true
}
