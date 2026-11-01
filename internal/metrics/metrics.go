// Package metrics is Cairn's Prometheus metrics registry (ADR-0021,
// SPEC-0014).
//
// # Metrics Registry
//
// cairnd builds one Registry at startup and hands it to the services that
// count things, as a constructor option, never a package global. Each metric
// has a small helper that takes domain values and maps them onto a closed
// label set, so no caller can put a client-supplied string, an artifact id or
// an actor into a label (SPEC-0014 REQ-5). Every helper is safe on a nil
// *Registry, which counts nothing, so a service built without metrics (a test,
// a tool) needs no stub.
//
// GET /metrics serves this Registry's Gatherer behind the operator credential
// (SPEC-0014 REQ-1). The storage and expiry gauges are computed at scrape time
// by the collector RegisterStorage installs (SPEC-0014 REQ-2); everything else
// is a counter incremented where the thing happens.
//
// Governing: ADR-0021, SPEC-0014 REQ-5; ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/25/2026 - Added for cairn#290 with cairn_redactions_total, the
// first metric. The wiring stories (#291-#293) call ObserveRedaction.
// @joestump-agent 10/30/2026 - Extended for cairn#256 with the SPEC-0014
// request, create, expiry and collection-error set, and the scrape-time
// storage collector.
package metrics

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/redact"
)

// Registry holds Cairn's metrics.
type Registry struct {
	reg        *prometheus.Registry
	redactions *prometheus.CounterVec

	// The SPEC-0014 request and create set. The request counter starts empty:
	// its label set is a hand-maintained route map, not an enumerable domain,
	// so the middleware mints series and a new route landing in "other" is the
	// classifier's own control (REQ-3).
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	created      *prometheus.CounterVec
	expired      prometheus.Counter
	collErrors   *collectionErrors

	// The scrape-time storage gauges, described here and collected by the
	// collector RegisterStorage installs. expiryRun reports only once a sweep
	// has completed without error: before that it is omitted, not 1970
	// (SPEC-0014 REQ-2).
	artifactsTotal *prometheus.Desc
	storageBytes   *prometheus.Desc
	pastTTL        *prometheus.Desc
	expiryRun      *expiryRunGauge
}

// New builds a Registry with every Cairn metric registered, each counter
// series initialized to zero so a dashboard sees the whole label set before
// the first event.
func New() *Registry {
	r := &Registry{
		reg: prometheus.NewRegistry(),
		redactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cairn_redactions_total",
			Help: "Ingest secret scans, by surface and outcome. Counts scans, not values: a field masked twice counts once.",
		}, []string{"surface", "outcome"}),
	}
	r.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cairn_http_requests_total",
		Help: "Requests by hand-maintained route class, method and status class. Never a raw path: artifact handles live in paths (SPEC-0014 REQ-3, REQ-5).",
	}, []string{"route_class", "method", "status_class"})
	r.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cairn_http_request_duration_seconds",
		Help:    "Request duration by hand-maintained route class (SPEC-0014 REQ-3).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"route_class"})
	r.created = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cairn_artifacts_created_total",
		Help: "Artifacts created, by share type and whether the client asserted the handoff tag (SPEC-0014 REQ-4). Only the boolean is a label: tags are client-asserted and unbounded (ADR-0018).",
	}, []string{"share_type", "tagged_handoff"})
	r.expired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "cairn_artifacts_expired_total",
		Help: "Artifacts the expiry sweep deleted. Counts what the sweeper removed, not what is overdue: a flat line here is ambiguous without cairn_artifacts_past_ttl (SPEC-0014 REQ-2).",
	})
	r.expiryRun = &expiryRunGauge{desc: prometheus.NewDesc(
		"cairn_expiry_run_timestamp",
		"Unix seconds of the last sweep that completed without error. Emitted only after one has: before the first successful sweep it is omitted, not 1970 (SPEC-0014 REQ-2).",
		nil, nil)}
	r.collErrors = newCollectionErrors()
	r.artifactsTotal = prometheus.NewDesc("cairn_artifacts_total", "Live artifacts by share type, computed at scrape time (SPEC-0014 REQ-2).", []string{"share_type"}, nil)
	r.storageBytes = prometheus.NewDesc("cairn_storage_bytes", "Bytes recorded on live artifact rows by share type, summed from the database rather than asked of the object store (SPEC-0014 REQ-2).", []string{"share_type"}, nil)
	r.pastTTL = prometheus.NewDesc("cairn_artifacts_past_ttl", "Live artifacts past their expires_at. The load-bearing gauge: it should sit at or near zero, and a collector that cannot compute it omits it rather than reporting zero (SPEC-0014 REQ-2, REQ-6).", nil, nil)

	r.reg.MustRegister(r.redactions, r.httpRequests, r.httpDuration,
		r.created, r.expired, r.expiryRun)
	for _, s := range RedactionSurfaces {
		for _, o := range RedactionOutcomes {
			r.redactions.WithLabelValues(string(s), string(o))
		}
	}
	return r
}

// Gatherer exposes the registry for the /metrics handler and for tests.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// MustRegister adds collectors the Registry does not itself own, such as
// cairn serve's go_* and process_* set (SPEC-0014 REQ-1). Startup-only, like the prometheus
// registry's own MustRegister: registering twice panics rather than silently
// serving a half-instrumented endpoint.
func (r *Registry) MustRegister(cs ...prometheus.Collector) {
	if r == nil {
		return
	}
	r.reg.MustRegister(cs...)
}

// RedactionSurface is the surface label of cairn_redactions_total: which kind
// of write was scanned (SPEC-0017 RD-4).
type RedactionSurface string

const (
	SurfaceArtifact RedactionSurface = "artifact" // a single-body artifact's body or title
	SurfaceBundle   RedactionSurface = "bundle"   // a bundle member or the bundle's title
	SurfaceComment  RedactionSurface = "comment"
	SurfaceRun      RedactionSurface = "run" // a run's title or prompt, or a span
	SurfaceWebhook  RedactionSurface = "webhook"
)

// RedactionSurfaces is the whole surface label set.
var RedactionSurfaces = []RedactionSurface{SurfaceArtifact, SurfaceBundle, SurfaceComment, SurfaceRun, SurfaceWebhook}

// RedactionOutcome is the outcome label of cairn_redactions_total.
type RedactionOutcome string

const (
	OutcomeClean              RedactionOutcome = "clean"
	OutcomeMasked             RedactionOutcome = "masked"
	OutcomeNotScannedBinary   RedactionOutcome = "not_scanned_binary"
	OutcomeNotScannedOversize RedactionOutcome = "not_scanned_oversize"
	OutcomeRejected           RedactionOutcome = "rejected"  // a secret was detected in reject mode, or could not be masked
	OutcomeTooLarge           RedactionOutcome = "too_large" // over the scan cap, and the oversize policy rejects
	OutcomeFailed             RedactionOutcome = "failed"    // the scan itself failed, so the write failed closed
)

// RedactionOutcomes is the whole outcome label set.
var RedactionOutcomes = []RedactionOutcome{
	OutcomeClean, OutcomeMasked, OutcomeNotScannedBinary, OutcomeNotScannedOversize,
	OutcomeRejected, OutcomeTooLarge, OutcomeFailed,
}

// RedactionOutcomeOf maps a scan's result, the Outcome and error that
// redact.Scanner.Text or Staged returned, to its label. ok is false for a
// result that is not a scan (a zero or "unscanned" Outcome with no error),
// which is not counted.
func RedactionOutcomeOf(o redact.Outcome, err error) (RedactionOutcome, bool) {
	if err != nil {
		switch {
		case errors.Is(err, redact.ErrScanFailed):
			return OutcomeFailed, true
		case errors.Is(err, redact.ErrTooLargeToScan):
			return OutcomeTooLarge, true
		case errors.Is(err, redact.ErrSecretDetected):
			return OutcomeRejected, true
		}
		return OutcomeFailed, true
	}
	switch o.Status {
	case redact.StatusClean:
		return OutcomeClean, true
	case redact.StatusMasked:
		return OutcomeMasked, true
	case redact.StatusNotScannedBinary:
		return OutcomeNotScannedBinary, true
	case redact.StatusNotScannedOversize:
		return OutcomeNotScannedOversize, true
	}
	return "", false
}

// HTTPRouteClass is the route_class label of cairn_http_requests_total and
// cairn_http_request_duration_seconds: the closed hand-maintained set of
// SPEC-0014 REQ-3. A route the map does not name is ClassOther, and that
// bucket rising is the map's own control.
type HTTPRouteClass string

const (
	ClassArtifactRead   HTTPRouteClass = "artifact_read"
	ClassArtifactCreate HTTPRouteClass = "artifact_create"
	ClassBundle         HTTPRouteClass = "bundle"
	ClassRun            HTTPRouteClass = "run"
	ClassComment        HTTPRouteClass = "comment"
	ClassWebhook        HTTPRouteClass = "webhook"
	ClassWeb            HTTPRouteClass = "web"
	ClassOther          HTTPRouteClass = "other"
)

// HTTPRouteClasses is the whole route_class label set.
var HTTPRouteClasses = []HTTPRouteClass{
	ClassArtifactRead, ClassArtifactCreate, ClassBundle, ClassRun,
	ClassComment, ClassWebhook, ClassWeb, ClassOther,
}

// StatusClassOf maps a status code onto its status_class label: the class,
// never the exact code (SPEC-0014 REQ-3).
func StatusClassOf(code int) string {
	switch {
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// ObserveHTTP counts one request and records its duration. A class outside
// HTTPRouteClasses counts as other rather than minting a new series
// (SPEC-0014 REQ-3, REQ-5).
func (r *Registry) ObserveHTTP(class HTTPRouteClass, method string, status int, seconds float64) {
	if r == nil {
		return
	}
	if !slices.Contains(HTTPRouteClasses, class) {
		class = ClassOther
	}
	r.httpRequests.WithLabelValues(string(class), method, StatusClassOf(status)).Inc()
	r.httpDuration.WithLabelValues(string(class)).Observe(seconds)
}

// ObserveArtifactCreated counts one committed artifact. tagged_handoff is the
// boolean "did the client assert the handoff tag", never the tag set: tags are
// client-asserted and unbounded, so they must not be labels (ADR-0018,
// SPEC-0014 REQ-4, REQ-5).
func (r *Registry) ObserveArtifactCreated(shareType artifact.ShareType, tags []string) {
	if r == nil {
		return
	}
	tagged := "false"
	if slices.Contains(tags, artifact.HandoffTag) {
		tagged = "true"
	}
	r.created.WithLabelValues(string(shareType), tagged).Inc()
}

// expiryRunGauge is cairn_expiry_run_timestamp as an honest-absence collector:
// it reports nothing until a sweep has completed without error, so the series
// is absent (not 1970, not 0) for an instance that has never finished a
// clean sweep (SPEC-0014 REQ-2).
type expiryRunGauge struct {
	desc *prometheus.Desc
	mu   sync.Mutex
	at   int64
}

func (g *expiryRunGauge) set(unixSeconds int64) {
	g.mu.Lock()
	g.at = unixSeconds
	g.mu.Unlock()
}

func (g *expiryRunGauge) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g *expiryRunGauge) Collect(ch chan<- prometheus.Metric) {
	g.mu.Lock()
	at := g.at
	g.mu.Unlock()
	if at == 0 {
		return
	}
	ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, float64(at))
}

// ObserveExpirySweep records one sweep that COMPLETED WITHOUT ERROR: the
// artifacts it deleted and the time it finished. An errored sweep must not
// call this. cairn_expiry_run_timestamp is what makes a flat
// cairn_artifacts_expired_total unambiguous, so advancing it on a failed sweep
// would erase the signal the pair exists to give (SPEC-0014 REQ-2).
func (r *Registry) ObserveExpirySweep(deleted int, at time.Time) {
	if r == nil {
		return
	}
	r.expired.Add(float64(deleted))
	r.expiryRun.set(at.Unix())
}

// ObserveCollectionError records that a named collector could not compute its
// values at this scrape, and so that whatever it would have reported is being
// OMITTED rather than reported as zero (SPEC-0014 REQ-6). It is called from
// inside Collect, which is why the counter is emitted by the collector that
// fails rather than read from a registered vector: see collectionErrors.
func (r *Registry) ObserveCollectionError(collector string) {
	if r == nil {
		return
	}
	r.collErrors.inc(collector)
}

// ObserveRedaction counts one scan on surface. Pass exactly what the scanner
// returned. A surface outside RedactionSurfaces, or a result that is not a
// scan, counts nothing rather than minting a new series.
func (r *Registry) ObserveRedaction(surface RedactionSurface, o redact.Outcome, err error) {
	if r == nil {
		return
	}
	known := false
	for _, s := range RedactionSurfaces {
		known = known || s == surface
	}
	outcome, ok := RedactionOutcomeOf(o, err)
	if !known || !ok {
		return
	}
	r.redactions.WithLabelValues(string(surface), string(outcome)).Inc()
}
