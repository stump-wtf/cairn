package trajectory

// The artifact store these tests seed through scans every create, and fails
// closed without a scanner (SPEC-0017 RD-1), so it gets the default one.
//
// @joestump 09/26/2026 - Added for cairn#292.

import (
	"testing"

	"github.com/stump-wtf/cairn/internal/redact"
)

func storeScanner(t *testing.T) *redact.Scanner {
	t.Helper()
	s, err := redact.New(redact.Config{})
	if err != nil {
		t.Fatalf("build the redaction scanner: %v", err)
	}
	return s
}
