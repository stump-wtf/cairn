package db

import (
	"reflect"
	"strings"
	"testing"
)

// TestIsNoTx pins the opt-out to an exact first line, so a directive quoted in
// a later comment, or a near miss, never silently drops a migration's
// transaction.
func TestIsNoTx(t *testing.T) {
	cases := map[string]bool{
		"-- cairn:no-transaction\nSELECT 1;":            true,
		"-- cairn:no-transaction  \r\nSELECT 1;":        true,
		"SELECT 1;\n-- cairn:no-transaction\n":          false,
		"-- cairn:no-transactions\nSELECT 1;":           false,
		"-- header\n-- cairn:no-transaction\nSELECT 1;": false,
		"": false,
	}
	for in, want := range cases {
		if got := isNoTx([]byte(in)); got != want {
			t.Errorf("isNoTx(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSplitStatements checks that a no-transaction migration is cut only on
// its break lines, that a DO block's internal semicolons survive, and that
// comment-only chunks (the file header) are not sent as statements.
func TestSplitStatements(t *testing.T) {
	in := strings.Join([]string{
		"-- cairn:no-transaction",
		"-- header prose; with a semicolon",
		"-- cairn:statement-break",
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT;",
		"  -- cairn:statement-break  ",
		"DO $$",
		"BEGIN",
		"  PERFORM 1; PERFORM 2;",
		"END $$;",
		"-- cairn:statement-break",
		"-- trailing comment only",
		"",
	}, "\n")
	got := splitStatements([]byte(in))
	want := []string{
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS c TEXT;",
		"DO $$\nBEGIN\n  PERFORM 1; PERFORM 2;\nEND $$;",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitStatements =\n%q\nwant\n%q", got, want)
	}

	// Positive control: without breaks the whole body is one statement.
	if got := splitStatements([]byte("SELECT 1;\nSELECT 2;\n")); len(got) != 1 {
		t.Fatalf("unbroken body split into %d statements, want 1", len(got))
	}
}
