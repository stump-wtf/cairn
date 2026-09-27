package metrics

// Redaction Metric Tests
//
// cairn_redactions_total counts real scanner results under a closed label set,
// ignores anything else, and never carries a scanned value. Credentials are
// assembled at run time from split literals.
//
// Governing: ADR-0021, SPEC-0014 REQ-5; ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/25/2026 - Added for cairn#290.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/stump-wtf/cairn/internal/redact"
)

func plantedToken(seed int) string {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < 36; i++ {
		b.WriteByte(alnum[(seed*11+i*7)%len(alnum)])
	}
	return "gh" + "p_" + b.String()
}

func count(r *Registry, s RedactionSurface, o RedactionOutcome) float64 {
	return testutil.ToFloat64(r.redactions.WithLabelValues(string(s), string(o)))
}

// exposition renders the registry the way /metrics will.
func exposition(t *testing.T, r *Registry) string {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

// TestRedactionSeriesInitialized: every surface x outcome series exists at
// zero before the first scan, and no other series does.
func TestRedactionSeriesInitialized(t *testing.T) {
	r := New()
	if n := testutil.CollectAndCount(r.redactions, "cairn_redactions_total"); n != len(RedactionSurfaces)*len(RedactionOutcomes) {
		t.Errorf("%d series, want %d", n, len(RedactionSurfaces)*len(RedactionOutcomes))
	}
	if got := count(r, SurfaceComment, OutcomeMasked); got != 0 {
		t.Errorf("fresh series = %v, want 0", got)
	}
}

// TestObserveRedactionFromScanner: each thing the scanner can return lands on
// its outcome label.
func TestObserveRedactionFromScanner(t *testing.T) {
	ctx := context.Background()
	sc, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	small, err := redact.New(redact.Config{MaxScanBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	storeUnscanned, err := redact.New(redact.Config{MaxScanBytes: 64, Oversize: redact.OversizeStoreUnscanned})
	if err != nil {
		t.Fatal(err)
	}
	tok := plantedToken(1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	type scan func() (string, redact.Outcome, error)
	cases := []struct {
		name string
		run  scan
		want RedactionOutcome
	}{
		{"clean", func() (string, redact.Outcome, error) { return sc.Text(ctx, "body", "nothing here", redact.ModeMask) }, OutcomeClean},
		{"masked", func() (string, redact.Outcome, error) { return sc.Text(ctx, "body", "t="+tok, redact.ModeMask) }, OutcomeMasked},
		{"rejected", func() (string, redact.Outcome, error) { return sc.Text(ctx, "body", "t="+tok, redact.ModeReject) }, OutcomeRejected},
		{"too large", func() (string, redact.Outcome, error) {
			return small.Text(ctx, "body", strings.Repeat("a", 65), redact.ModeMask)
		}, OutcomeTooLarge},
		{"stored unscanned", func() (string, redact.Outcome, error) {
			return storeUnscanned.Text(ctx, "body", strings.Repeat("a", 65), redact.ModeMask)
		}, OutcomeNotScannedOversize},
		{"failed", func() (string, redact.Outcome, error) { return sc.Text(cancelled, "body", "t="+tok, redact.ModeMask) }, OutcomeFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New()
			_, o, err := c.run()
			r.ObserveRedaction(SurfaceArtifact, o, err)
			if got := count(r, SurfaceArtifact, c.want); got != 1 {
				t.Errorf("%s = %v, want 1 (err %v)", c.want, got, err)
			}
			if strings.Contains(exposition(t, r), tok[4:24]) {
				t.Error("the metrics exposition holds part of the planted token")
			}
		})
	}
	if got, ok := RedactionOutcomeOf(redact.Outcome{Status: redact.StatusNotScannedBinary}, nil); !ok || got != OutcomeNotScannedBinary {
		t.Errorf("binary maps to %q, %v", got, ok)
	}
}

// TestObserveRedactionIgnoresNonScans: an unknown surface or an outcome that
// is not a scan mints no series, and a nil Registry is inert.
func TestObserveRedactionIgnoresNonScans(t *testing.T) {
	r := New()
	r.ObserveRedaction("artifact/"+RedactionSurface(plantedToken(2)), redact.Outcome{Status: redact.StatusClean}, nil)
	r.ObserveRedaction(SurfaceRun, redact.Outcome{}, nil)
	r.ObserveRedaction(SurfaceRun, redact.Outcome{Status: redact.StatusUnscanned}, nil)
	if n := testutil.CollectAndCount(r.redactions, "cairn_redactions_total"); n != len(RedactionSurfaces)*len(RedactionOutcomes) {
		t.Errorf("a non-scan minted a series: %d", n)
	}
	for _, s := range RedactionSurfaces {
		for _, o := range RedactionOutcomes {
			if got := count(r, s, o); got != 0 {
				t.Errorf("%s/%s = %v, want 0", s, o, got)
			}
		}
	}
	var nilReg *Registry
	nilReg.ObserveRedaction(SurfaceRun, redact.Outcome{Status: redact.StatusClean}, nil)
}
