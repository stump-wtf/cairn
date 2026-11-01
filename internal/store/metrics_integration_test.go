package store

// Storage and Expiry Metric Tests
//
// SPEC-0014 REQ-2 and REQ-6, against the real artifacts table: the gauges are
// computed at scrape time, so what they must track is the database's own state
// rather than anything a code path remembered to increment. The last test is
// the spec's first scenario end to end: the sweep goes broken, and the three
// signals separate "broken" from "nothing to do".
//
// Governing: ADR-0021, SPEC-0014 REQ-2, REQ-6; SPEC-0009 (the reaper)
//
// @joestump-agent 10/30/2026 - Added for cairn#256.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/common/expfmt"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/metrics"
	"github.com/stump-wtf/cairn/internal/objectstore"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// scrape renders the registry the way GET /metrics will.
func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buf strings.Builder
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

// expiredInput is input() with an expiry already in the past: an artifact the
// next clean sweep should remove, and one the gauges must count as overdue
// until it does.
func expiredInput(body []byte) CreateArtifactInput {
	return inputTTL(body, time.Now().Add(-time.Minute))
}

func mustCreate(t *testing.T, s *Store, in CreateArtifactInput) *artifact.Artifact {
	t.Helper()
	a, err := s.CreateArtifact(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return a
}

// registerMetrics wires the scrape-time collector over this store, the way
// cairn serve does at startup.
func registerMetrics(t *testing.T, s *Store, reg *metrics.Registry) {
	t.Helper()
	reg.RegisterStorage(s, sharetype.Default().KeyedTypes())
}

// quietLogger keeps the reaper's own logging out of test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sampleLine returns the exposition line for a metric family with no labels, or
// "" when it is absent. Substring matching is not enough here: "# HELP
// cairn_artifacts_past_ttl …" contains the family name too, so a test that
// asserts absence by Contains passes on a scrape that reported nothing.
func sampleLine(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name) && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// breakTheSweeper makes every DELETE against artifacts fail while SELECT keeps
// working. That asymmetry is the point: it is how a real instance's sweeper goes
// broken (missing grant, failing trigger, read replica) without taking the read
// path, and therefore the gauges, down with it, which is exactly the state
// SPEC-0014 REQ-2 asks the three signals to distinguish.
func breakTheSweeper(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		CREATE FUNCTION cairn_break_the_sweeper() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN RAISE EXCEPTION 'injected: the sweeper cannot write'; END
		$fn$;
		CREATE TRIGGER cairn_break_the_sweeper BEFORE DELETE ON artifacts
			FOR EACH ROW EXECUTE FUNCTION cairn_break_the_sweeper()`)
	if err != nil {
		t.Fatalf("break sweeper: %v", err)
	}
}

// newBlindStore is a store whose every query fails, because its search_path
// names a schema that was never created. The collectors see what an instance
// with an unmigrated or unreachable database sees: an error, never a zero.
func newBlindStore(t *testing.T, reg *metrics.Registry) *Store {
	t.Helper()
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CAIRN_TEST_DATABASE_URL to run store integration tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "cairn_metrics_no_such_schema"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool, objectstore.NewMemory(), Options{Metrics: reg})
}

// TestGaugesTrackTheDatabase: the storage gauges describe the rows, computed at
// scrape time, and report every registered share type including the empty ones.
func TestGaugesTrackTheDatabase(t *testing.T) {
	reg := metrics.New()
	s, _ := newTestStore(t, Options{Metrics: reg})
	registerMetrics(t, s, reg)

	mustCreate(t, s, input([]byte("hello")))
	handoff := input([]byte("work order"))
	handoff.Tags = []string{"handoff", "lane:m"}
	mustCreate(t, s, handoff)
	mustCreate(t, s, inputTTL([]byte("gone soon"), time.Now().Add(-time.Hour)))

	body := scrape(t, reg)
	for _, want := range []string{
		`cairn_artifacts_total{share_type="file"} 3`,
		`cairn_storage_bytes{share_type="file"} 24`,
		// Registered and empty: reported at zero, not absent.
		`cairn_artifacts_total{share_type="markdown"} 0`,
		`cairn_artifacts_past_ttl 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
	// The create counter splits on the handoff boolean only: no tag value, and
	// no artifact handle, appears anywhere (SPEC-0014 REQ-4, REQ-5).
	if !strings.Contains(body, `cairn_artifacts_created_total{share_type="file",tagged_handoff="true"} 1`) {
		t.Errorf("handoff-tagged create not counted:\n%s", body)
	}
	for _, leak := range []string{"lane:m", "handoff\"", "cairn_artifacts_total{handle"} {
		if strings.Contains(body, leak) {
			t.Errorf("%q reached the exposition:\n%s", leak, body)
		}
	}
}

// TestBrokenSweeperThreeSignals is SPEC-0014's scenario "the expiry sweep
// silently stops": expired_total flat, expiry_run_timestamp frozen, past_ttl
// climbing. Flat alone reads as a quiet period; frozen-and-climbing does not.
// The sweep is broken the way an instance's would be, its database stops
// answering, after one clean sweep has established the baseline.
func TestBrokenSweeperThreeSignals(t *testing.T) {
	reg := metrics.New()
	s, pool := newTestStore(t, Options{Metrics: reg})
	registerMetrics(t, s, reg)

	mustCreate(t, s, expiredInput([]byte("one")))
	s.sweepOnce(context.Background(), ReaperConfig{}, quietLogger())

	frozen := scrape(t, reg)
	if !strings.Contains(frozen, "cairn_artifacts_expired_total 1") {
		t.Fatalf("the clean sweep established no baseline:\n%s", frozen)
	}
	frozenRun := sampleLine(frozen, "cairn_expiry_run_timestamp")
	if frozenRun == "" {
		t.Fatalf("clean sweep left no cairn_expiry_run_timestamp:\n%s", frozen)
	}

	breakTheSweeper(t, pool)
	mustCreate(t, s, expiredInput([]byte("two")))
	mustCreate(t, s, expiredInput([]byte("three")))
	s.sweepOnce(context.Background(), ReaperConfig{}, quietLogger())

	body := scrape(t, reg)
	if got := sampleLine(body, "cairn_expiry_run_timestamp"); got != frozenRun {
		t.Errorf("expiry_run_timestamp moved on a sweep that never completed: %q, want %q", got, frozenRun)
	}
	if !strings.Contains(body, "cairn_artifacts_expired_total 1") {
		t.Errorf("expired_total moved on a sweep that never completed:\n%s", body)
	}
	if got := sampleLine(body, "cairn_artifacts_past_ttl"); got != "cairn_artifacts_past_ttl 2" {
		t.Errorf("past_ttl did not climb while the sweeper was broken: %q", got)
	}
	// A broken SWEEP is not a broken COLLECTOR: the gauges still answered.
	for _, collector := range metrics.StorageCollectors {
		want := `cairn_metrics_collection_errors_total{collector="` + collector + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("collection errors recorded for a sweep failure:\n%s", body)
		}
	}
}

// TestGaugesOmittedWhenTheStoreIsBlind is REQ-6 with a real store: when the
// database cannot answer, the gauges are absent and the error counter says
// why. Absent, because cairn_artifacts_past_ttl at zero means "nothing is
// overdue", and that is the last thing an instance with a blind collector
// should appear to assert.
func TestGaugesOmittedWhenTheStoreIsBlind(t *testing.T) {
	reg := metrics.New()
	registerMetrics(t, newBlindStore(t, reg), reg)

	body := scrape(t, reg)
	if strings.Contains(body, "cairn_artifacts_total{") || strings.Contains(body, "cairn_storage_bytes{") {
		t.Errorf("storage gauges reported from an unanswerable store:\n%s", body)
	}
	if got := sampleLine(body, "cairn_artifacts_past_ttl"); got != "" {
		t.Errorf("past_ttl reported from an unanswerable store: %q", got)
	}
	for _, want := range []string{
		`cairn_metrics_collection_errors_total{collector="storage_counts"} 1`,
		`cairn_metrics_collection_errors_total{collector="past_ttl"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
}

// TestExpiryRunTimestampAbsentBeforeFirstSweep keeps the honest-absence rule at
// the store seam too: an instance that has not finished a clean sweep reports
// nothing, not the epoch.
func TestExpiryRunTimestampAbsentBeforeFirstSweep(t *testing.T) {
	reg := metrics.New()
	s, _ := newTestStore(t, Options{Metrics: reg})
	registerMetrics(t, s, reg)

	if body := scrape(t, reg); strings.Contains(body, "cairn_expiry_run_timestamp ") {
		t.Errorf("expiry_run_timestamp present before any sweep:\n%s", body)
	}
}
