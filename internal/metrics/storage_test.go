package metrics

// Storage and Expiry Gauge Tests
//
// The gauges computed at scrape time are the part of SPEC-0014 whose failure
// mode is "looks healthy while lying", so every test here asserts a SHAPE on
// the exposition rather than a number: what is present, what is absent, and
// what moves when the sweeper stops working.
//
// Governing: ADR-0021, SPEC-0014 REQ-2, REQ-6
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stump-wtf/cairn/internal/artifact"
)

var errBlind = errors.New("storage stats: database is down")

// fakeStorage is the store seam the collector reads: a controllable snapshot
// with a switch for going blind.
type fakeStorage struct {
	byType  map[artifact.ShareType]TypeUsage
	pastTTL int
	usageEr error
	ttlEr   error
}

func (f *fakeStorage) StorageUsageByType(context.Context) (map[artifact.ShareType]TypeUsage, error) {
	return f.byType, f.usageEr
}

func (f *fakeStorage) CountArtifactsPastTTL(context.Context) (int, error) {
	return f.pastTTL, f.ttlEr
}

// knownTypes is the closed share_type set a real registry would hand
// RegisterStorage.
func knownTypes() []artifact.ShareType {
	return []artifact.ShareType{"code", "file", "markdown"}
}

func gauge(t *testing.T, r *Registry, name string) (float64, bool) {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			return 0, false
		}
		return mf.GetMetric()[0].GetGauge().GetValue(), true
	}
	return 0, false
}

func counter(t *testing.T, r *Registry, name string) (float64, bool) {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range mf.GetMetric() {
			sum += m.GetCounter().GetValue()
		}
		return sum, true
	}
	return 0, false
}

// seriesCount counts the series one metric family exposes at this scrape.
func seriesCount(t *testing.T, r *Registry, name string) int {
	t.Helper()
	mfs, err := r.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

// TestStorageGaugesReportEveryKnownType: a scrape reports the whole closed
// label set, zeros included, so a dashboard can tell "no markdown artifacts"
// from "markdown was never measured (SPEC-0014 REQ-2).
func TestStorageGaugesReportEveryKnownType(t *testing.T) {
	r := New()
	r.RegisterStorage(&fakeStorage{byType: map[artifact.ShareType]TypeUsage{
		"markdown": {Count: 3, Bytes: 300},
	}}, knownTypes())

	if n := seriesCount(t, r, "cairn_artifacts_total"); n != 3 {
		t.Errorf("cairn_artifacts_total has %d series, want 3 (every known type)", n)
	}
	if n := seriesCount(t, r, "cairn_storage_bytes"); n != 3 {
		t.Errorf("cairn_storage_bytes has %d series, want 3", n)
	}
	body := exposition(t, r)
	if !strings.Contains(body, `cairn_artifacts_total{share_type="code"} 0`) {
		t.Errorf("a known type with no rows is not reported at zero:\n%s", body)
	}
	// A healthy scrape records no collection failure: the counter is there,
	// because its label set is closed and initialized at startup, and it is
	// zero for every collector.
	if got, ok := counter(t, r, "cairn_metrics_collection_errors_total"); !ok || got != 0 {
		t.Errorf("collection errors = %v, present=%v on a healthy scrape; want 0, true", got, ok)
	}
}

// hasSample reports whether the exposition carries a SAMPLE line for the
// named family. Matching the bare name is not enough: it also appears in
// another family's HELP text.
func hasSample(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name) && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}

// TestPastTTLAbsentWhenCollectorFails is REQ-6's load-bearing case: a failed
// collector omits cairn_artifacts_past_ttl rather than reporting zero, because
// zero is the healthy reading and would assert health at the exact moment the
// collector went blind. The error counter is what replaces the missing value.
// One scrape, one set of assertions on it: each scrape of a blind collector is
// another recorded failure, which is the intended behaviour but would make a
// counted assertion scrape-order dependent.
func TestPastTTLAbsentWhenCollectorFails(t *testing.T) {
	r := New()
	st := &fakeStorage{byType: map[artifact.ShareType]TypeUsage{"markdown": {Count: 7, Bytes: 70}}, pastTTL: 4}
	r.RegisterStorage(st, knownTypes())

	if v, ok := gauge(t, r, "cairn_artifacts_past_ttl"); !ok || v != 4 {
		t.Fatalf("past_ttl = %v, present=%v before the failure; want 4, true", v, ok)
	}

	st.ttlEr = errBlind
	body := exposition(t, r)
	if hasSample(body, "cairn_artifacts_past_ttl") {
		t.Errorf("past_ttl reported while its collector failed; it MUST be omitted, never zero:\n%s", body)
	}
	if !strings.Contains(body, `cairn_metrics_collection_errors_total{collector="past_ttl"}`) {
		t.Errorf("the failed collector recorded nothing:\n%s", body)
	}
	// The storage gauges answer independently: the failure was the TTL query.
	if n := seriesCount(t, r, "cairn_artifacts_total"); n != 3 {
		t.Errorf("storage gauges disappeared with an unrelated collector failure: %d series", n)
	}
}

// TestStorageGaugesAbsentWhenCollectorFails: the same honesty for the counts:
// zero would read as an empty store.
func TestStorageGaugesAbsentWhenCollectorFails(t *testing.T) {
	r := New()
	st := &fakeStorage{byType: map[artifact.ShareType]TypeUsage{"markdown": {Count: 7, Bytes: 70}}, pastTTL: 0}
	r.RegisterStorage(st, knownTypes())
	st.usageEr = errBlind

	body := exposition(t, r)
	if strings.Contains(body, "cairn_artifacts_total{") || strings.Contains(body, "cairn_storage_bytes{") {
		t.Errorf("storage gauges reported while their collector failed:\n%s", body)
	}
	if hasSample(body, "cairn_artifacts_past_ttl") == false {
		t.Errorf("past_ttl vanished with the storage collector; the two fail independently:\n%s", body)
	}
	if !strings.Contains(body, `cairn_metrics_collection_errors_total{collector="storage_counts"}`) {
		t.Errorf("the failed collector recorded nothing:\n%s", body)
	}
}

// TestExpiryRunTimestampAbsentBeforeFirstSweep: absent, not 1970 (REQ-2).
func TestExpiryRunTimestampAbsentBeforeFirstSweep(t *testing.T) {
	r := New()
	if _, ok := gauge(t, r, "cairn_expiry_run_timestamp"); ok {
		t.Error("cairn_expiry_run_timestamp present before any sweep; it must be omitted, not 1970")
	}
}

// TestExpiryRunTimestampFrozenOnFailedSweep: an errored sweep advances nothing.
func TestExpiryRunTimestampFrozenOnFailedSweep(t *testing.T) {
	r := New()
	at := time.Unix(1_760_000_000, 0)
	r.ObserveExpirySweep(2, at)
	r.ObserveExpirySweep(0, at.Add(time.Hour))

	if v, ok := gauge(t, r, "cairn_expiry_run_timestamp"); !ok || v != float64(at.Add(time.Hour).Unix()) {
		t.Errorf("expiry_run_timestamp = %v, present=%v; want %v", v, ok, at.Add(time.Hour).Unix())
	}
	if got, _ := counter(t, r, "cairn_artifacts_expired_total"); got != 2 {
		t.Errorf("expired_total = %v, want 2", got)
	}
}

// TestBrokenSweeperThreeSignals is SPEC-0014's first scenario in one place:
// the sweep starts failing, and the three signals separate "broken" from
// "quiet": expired_total flat, expiry_run_timestamp frozen, past_ttl
// climbing. Flat alone is ambiguous; frozen + climbing is not.
func TestBrokenSweeperThreeSignals(t *testing.T) {
	r := New()
	st := &fakeStorage{byType: map[artifact.ShareType]TypeUsage{}, pastTTL: 0}
	r.RegisterStorage(st, knownTypes())

	sweep := func(at time.Time, err error) {
		if err != nil {
			return // an errored sweep is recorded nowhere
		}
		r.ObserveExpirySweep(1, at)
	}

	t0 := time.Unix(1_760_000_000, 0)
	sweep(t0, nil) // one clean sweep: it ran, and it worked
	st.byType["file"] = TypeUsage{Count: 1, Bytes: 10}
	st.pastTTL = 1
	sweep(t0.Add(time.Minute), errBlind)
	st.byType["markdown"] = TypeUsage{Count: 2, Bytes: 20}
	st.pastTTL = 2
	sweep(t0.Add(2*time.Minute), errBlind)

	if v, _ := gauge(t, r, "cairn_expiry_run_timestamp"); v != float64(t0.Unix()) {
		t.Errorf("expiry_run_timestamp advanced on failed sweeps: %v, want frozen at %v", v, t0.Unix())
	}
	if got, _ := counter(t, r, "cairn_artifacts_expired_total"); got != 1 {
		t.Errorf("expired_total = %v, want 1 (flat since the last clean sweep)", got)
	}
	if v, _ := gauge(t, r, "cairn_artifacts_past_ttl"); v != 2 {
		t.Errorf("past_ttl = %v, want 2 (climbing while the sweeper is broken)", v)
	}
	if got, ok := counter(t, r, "cairn_metrics_collection_errors_total"); !ok || got != 0 {
		t.Errorf("collection errors = %v, present=%v; want 0, true: a failing SWEEP is not a failing COLLECTOR", got, ok)
	}
}

// TestObserveHTTPRouteClassIsClosed: an off-set class counts as other rather
// than minting a series, and the status label is a class, not the code.
func TestObserveHTTPRouteClassIsClosed(t *testing.T) {
	r := New()
	r.ObserveHTTP(HTTPRouteClass("/artifacts/HnI3onbw"), "GET", 404, 0.01)
	r.ObserveHTTP(ClassArtifactRead, "GET", 200, 0.01)

	body := exposition(t, r)
	if strings.Contains(body, "HnI3onbw") {
		t.Errorf("a path reached a label:\n%s", body)
	}
	if !strings.Contains(body, `cairn_http_requests_total{method="GET",route_class="other",status_class="4xx"} 1`) {
		t.Errorf("an off-set class did not count as other:\n%s", body)
	}
	if strings.Contains(body, `status_class="404"`) {
		t.Errorf("the exact status code became a label:\n%s", body)
	}
}

// TestObserveArtifactCreatedHandoffBoolean: REQ-4's label is the boolean
// "carried the handoff tag", never the tag set.
func TestObserveArtifactCreatedHandoffBoolean(t *testing.T) {
	r := New()
	r.ObserveArtifactCreated("markdown", []string{"handoff", "lane:m", "repo:stump.wtf/cairn"})
	r.ObserveArtifactCreated("markdown", []string{"todo"})

	if got := testutil.ToFloat64(r.created.WithLabelValues("markdown", "true")); got != 1 {
		t.Errorf("tagged_handoff=true = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.created.WithLabelValues("markdown", "false")); got != 1 {
		t.Errorf("tagged_handoff=false = %v, want 1", got)
	}
	body := exposition(t, r)
	for _, leak := range []string{"lane:m", "repo:stump.wtf", "lane"} {
		if strings.Contains(body, leak) {
			t.Errorf("tag value %q reached the exposition:\n%s", leak, body)
		}
	}
}

// TestNilRegistryInert keeps the package's nil-safe contract true for the new
// helpers.
func TestNilRegistryInert(t *testing.T) {
	var r *Registry
	r.ObserveHTTP(ClassWeb, "GET", 200, 0.1)
	r.ObserveArtifactCreated("code", []string{"handoff"})
	r.ObserveExpirySweep(3, time.Now())
	r.ObserveCollectionError(CollectorPastTTL)
	r.RegisterStorage(&fakeStorage{}, knownTypes())
}
