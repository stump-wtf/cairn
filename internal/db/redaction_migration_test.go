package db

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/redact"
)

// TestRedactionStatusConstraintMatchesDomain pins the redaction_status CHECK on
// every outcome table to redact.Statuses. If they drift, the scanner produces
// a status the database refuses, and the write fails after the scan passed.
// It also pins the 'unscanned' default that makes legacy rows honest.
//
// Governing: ADR-0023, SPEC-0017 RD-9, "Database Operation Standards"
func TestRedactionStatusConstraintMatchesDomain(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*_redaction_outcome.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("want exactly one *_redaction_outcome.sql migration, got %v (%v)", names, err)
	}
	raw, err := migrationsFS.ReadFile(names[0])
	if err != nil {
		t.Fatal(err)
	}
	sql := string(stripSQLComments(raw))

	want := make([]string, 0, len(redact.Statuses))
	for _, s := range redact.Statuses {
		want = append(want, string(s))
	}
	slices.Sort(want)

	checks := regexp.MustCompile(`(\w+)_redaction_status_chk CHECK \(redaction_status IN\s*\(([^)]*)\)\)`).FindAllStringSubmatch(sql, -1)
	var tables []string
	for _, m := range checks {
		tables = append(tables, m[1])
		var got []string
		for _, v := range strings.Split(m[2], ",") {
			got = append(got, strings.Trim(strings.TrimSpace(v), "'"))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s allows %v, redact.Statuses is %v", m[1], got, want)
		}
	}
	slices.Sort(tables)
	if wantTables := []string{"artifacts", "bundle_members", "comments", "runs"}; !slices.Equal(tables, wantTables) {
		t.Errorf("status CHECKs on %v, want %v", tables, wantTables)
	}
	if n := strings.Count(sql, "redaction_status TEXT NOT NULL DEFAULT 'unscanned'"); n != 4 {
		t.Errorf("%d tables default redaction_status to 'unscanned', want 4", n)
	}
}
