package redact

import "testing"

// newTestScanner builds a Scanner with the production defaults.
func newTestScanner(t testing.TB) *Scanner {
	t.Helper()
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}
