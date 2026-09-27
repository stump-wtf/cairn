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
// The registry is not served yet. GET /metrics, the storage and expiry set and
// the Go collectors arrive with #256, which serves this Registry's Gatherer.
//
// Governing: ADR-0021, SPEC-0014 REQ-5; ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/25/2026 - Added for cairn#290 with cairn_redactions_total, the
// first metric. The wiring stories (#291-#293) call ObserveRedaction.
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/cairn/internal/redact"
)

// Registry holds Cairn's metrics.
type Registry struct {
	reg        *prometheus.Registry
	redactions *prometheus.CounterVec
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
	r.reg.MustRegister(r.redactions)
	for _, s := range RedactionSurfaces {
		for _, o := range RedactionOutcomes {
			r.redactions.WithLabelValues(string(s), string(o))
		}
	}
	return r
}

// Gatherer exposes the registry for the /metrics handler and for tests.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

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
