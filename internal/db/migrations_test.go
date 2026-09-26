package db

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stump-wtf/cairn/internal/trajectory"
)

// TestSpanCategoryConstraintMatchesDomain pins the span-category length ceiling
// to a single number across the two layers that enforce it. 0013 opened the
// category set but kept a bound; trajectory.MaxCategoryLen enforces the same
// bound in the service. If they drift, one layer rejects what the other accepts
// — which is precisely the failure issue #5 was filed for, where the schema
// enforced a closed enum the service and the spec had already opened.
//
// Governing: ADR-0009 (open category set), SPEC-0004 REQ "Category length bounded"
func TestSpanCategoryConstraintMatchesDomain(t *testing.T) {
	raw, err := migrationsFS.ReadFile("migrations/0013_span_category_open_enum.sql")
	if err != nil {
		t.Fatalf("read 0013: %v", err)
	}
	// Match against DDL only. 0013's header quotes the closed enum it removes,
	// and the COMMENT ON strings list the recommended values — neither is a
	// constraint, and both would otherwise trip the assertions below.
	sql := stripSQLComments(raw)

	m := regexp.MustCompile(`length\(category\)\s*<=\s*(\d+)`).FindSubmatch(sql)
	if m == nil {
		t.Fatal("0013 no longer bounds length(category); the service still does, so the two layers have drifted")
	}
	if got, want := string(m[1]), strconv.Itoa(trajectory.MaxCategoryLen); got != want {
		t.Fatalf("0013 bounds category at %s, trajectory.MaxCategoryLen is %s", got, want)
	}

	// The closed enum 0004 shipped must stay dropped: re-adding a value list
	// would silently re-close the set behind the service's back.
	if regexp.MustCompile(`category\s+IN\s*\(`).Match(sql) {
		t.Fatal("0013 reintroduces a closed `category IN (...)` list; the set is open (ADR-0009)")
	}
}

// stripSQLComments drops `--` line comments so an assertion reads the migration's
// DDL rather than its prose.
func stripSQLComments(sql []byte) []byte {
	return regexp.MustCompile(`(?m)--.*$`).ReplaceAll(sql, nil)
}
