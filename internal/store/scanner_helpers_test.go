package store

// Test Scanner
//
// Every store a test builds scans, as cairnd's does: a Store without a scanner
// fails every create closed (scan.go), so the harnesses give it the real one,
// built with the default configuration, unless a test brings its own.
//
// Governing: ADR-0023, SPEC-0017 RD-1
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"sync"
	"testing"

	"github.com/stump-wtf/cairn/internal/redact"
)

var defaultTestScanner = sync.OnceValues(func() (*redact.Scanner, error) {
	return redact.New(redact.Config{})
})

// testScanner is the shared default scanner; the detector is safe for
// concurrent use.
func testScanner(t testing.TB) *redact.Scanner {
	t.Helper()
	s, err := defaultTestScanner()
	if err != nil {
		t.Fatalf("build the redaction scanner: %v", err)
	}
	return s
}

// withScanner fills in the default scanner when o has none.
func withScanner(t testing.TB, o Options) Options {
	t.Helper()
	if o.Scanner == nil {
		o.Scanner = testScanner(t)
	}
	return o
}
