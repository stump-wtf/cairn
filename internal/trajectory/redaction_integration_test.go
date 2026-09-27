package trajectory

import (
	"context"
	"testing"
)

// TestUnwiredRunRecordsUnscanned: a run's scan outcome is written with the run,
// and until #291 wires the scanner in it is "unscanned", never "clean".
//
// Governing: ADR-0023, SPEC-0017 RD-9
func TestUnwiredRunRecordsUnscanned(t *testing.T) {
	svc, st, pool, _ := newHarness(t)
	ctx := context.Background()

	run, err := svc.CreateBatchRun(ctx, checkoutWebAudit(createMarkdown(t, st)))
	if err != nil {
		t.Fatal(err)
	}
	var (
		status string
		count  int
		rules  map[string]int
	)
	if err := pool.QueryRow(ctx, `
		SELECT r.redaction_status, r.redaction_count, r.redaction_rules
		FROM runs r JOIN artifacts a ON a.id = r.artifact_id
		WHERE a.public_id = $1`, run.PublicID).Scan(&status, &count, &rules); err != nil {
		t.Fatal(err)
	}
	if status != "unscanned" || count != 0 || rules == nil || len(rules) != 0 {
		t.Errorf("run outcome = %s, %d, %v; want unscanned, 0, {}", status, count, rules)
	}
}
