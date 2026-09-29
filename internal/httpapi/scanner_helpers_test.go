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
// @joestump 09/27/2026 - One scanner per test binary: testScanner shares
// sharedTestScanner's instance (#293) instead of building a second.

import (
	"github.com/stump-wtf/cairn/internal/redact"
	"github.com/stump-wtf/cairn/internal/store"
)

// testScanner is the shared default scanner, the same instance
// sharedTestScanner hands the test servers' Config.Redaction, for callers
// with no testing.TB. Building it cannot fail without a
// configuration, so a failure is a broken build and panics.
func testScanner() *redact.Scanner {
	sharedScannerOnce.Do(func() { sharedScanner, sharedScannerErr = redact.New(redact.Config{}) })
	if sharedScannerErr != nil {
		panic("build the redaction scanner: " + sharedScannerErr.Error())
	}
	return sharedScanner
}

// withScanner fills in the default scanner when opts has none.
func withScanner(opts store.Options) store.Options {
	if opts.Scanner == nil {
		opts.Scanner = testScanner()
	}
	return opts
}
