package httpapi

// Test Scanner
//
// Every store the httpapi tests build scans creates, as cairnd's does: a
// store without a scanner fails every create closed, so the harnesses give it
// the real one, built with the default configuration, unless a test brings
// its own.
//
// Governing: ADR-0023, SPEC-0017 RD-1
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"sync"

	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

var defaultTestScanner = sync.OnceValues(func() (*redact.Scanner, error) {
	return redact.New(redact.Config{})
})

// testScanner is the shared default scanner. Building it cannot fail without
// a configuration, so a failure is a broken build and panics.
func testScanner() *redact.Scanner {
	s, err := defaultTestScanner()
	if err != nil {
		panic("build the redaction scanner: " + err.Error())
	}
	return s
}

// withScanner fills in the default scanner when opts has none.
func withScanner(opts store.Options) store.Options {
	if opts.Scanner == nil {
		opts.Scanner = testScanner()
	}
	return opts
}
