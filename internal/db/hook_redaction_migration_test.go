package db

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/redact"
)

// TestHookRequestRedactionConstraintMatchesDomain pins hook_requests'
// redaction_status CHECK to redact.Statuses and its withheld CHECK to the
// captured fields the webhook scanner can withhold, the same drift guard
// TestRedactionStatusConstraintMatchesDomain is for the 0017 tables. If they
// drift, a capture's scan passes and its insert then fails, and the sender's
// request is lost.
//
// Governing: ADR-0023, SPEC-0017 RD-4, RD-9, "Database Operation Standards"
func TestHookRequestRedactionConstraintMatchesDomain(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*_hook_request_redaction.sql")
	if err != nil || len(names) != 1 {
		t.Fatalf("want exactly one *_hook_request_redaction.sql migration, got %v (%v)", names, err)
	}
	raw, err := migrationsFS.ReadFile(names[0])
	if err != nil {
		t.Fatal(err)
	}
	sql := string(stripSQLComments(raw))

	list := func(re string) []string {
		m := regexp.MustCompile(re).FindStringSubmatch(sql)
		if m == nil {
			t.Fatalf("no match for %s", re)
		}
		var got []string
		for _, v := range strings.Split(m[1], ",") {
			got = append(got, strings.Trim(strings.TrimSpace(v), "'"))
		}
		slices.Sort(got)
		return got
	}

	want := make([]string, 0, len(redact.Statuses))
	for _, s := range redact.Statuses {
		want = append(want, string(s))
	}
	slices.Sort(want)
	if got := list(`hook_requests_redaction_status_chk CHECK \(redaction_status IN\s*\(([^)]*)\)\)`); !slices.Equal(got, want) {
		t.Errorf("hook_requests allows statuses %v, redact.Statuses is %v", got, want)
	}
	// The webhook package's capturedFields; kept literal here so the db
	// package does not import webhook.
	if got := list(`redaction_withheld <@ ARRAY\[([^\]]*)\]`); !slices.Equal(got, []string{"body", "headers", "query"}) {
		t.Errorf("hook_requests withheld fields %v, want [body headers query]", got)
	}
	if !strings.Contains(sql, "redaction_status   TEXT   NOT NULL DEFAULT 'unscanned'") {
		t.Error("hook_requests.redaction_status must default to 'unscanned' so pre-scanner captures read honestly")
	}
}
