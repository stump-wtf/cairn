package clicmd

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// Ingest Redaction in the CLI
//
// The server scans every create for credentials. Code and bundles refuse a
// hit by default; `--redact=mask` downgrades that to storing the value as
// [REDACTED], sent as X-Cairn-Redaction: mask. "mask" is the only value there
// is, so anything else is a usage error before any network call: nothing the
// CLI sends can turn the scan off (SPEC-0017 RD-5).
//
// Whenever the server did mask something, the stored bytes, and so their
// SHA-256, differ from the input. The CLI says so on stderr, where it cannot
// break a pipe or a --json reader, from the outcome the response reports
// (RD-9, RD-10). The count and rule IDs are all it has; no value ever
// reaches the CLI.
//
// Governing: ADR-0023, SPEC-0017 RD-5, RD-9, RD-10, RD-11; SPEC-0008 REQ
// "Pipe and Path Ingest", REQ "Bundle Creation"

// parseRedactFlag checks `--redact`. set reports whether the flag was given
// at all, so an explicit empty value is refused like any other.
func parseRedactFlag(value string, set bool) (string, error) {
	if !set || value == cliclient.RedactionMask {
		return value, nil
	}
	return "", usageErrorf("--redact %s is not valid: the only value is %s, and scanning cannot be turned off",
		shellWord(value), cliclient.RedactionMask)
}

// warnRedacted prints one warning line to errOut when the server masked any
// value in art, e.g.
//
//	cairn: warning: 2 values masked (github-pat ×2); stored content differs from input
//
// It prints nothing for a clean create or a server that predates scanning.
func warnRedacted(errOut io.Writer, art *cliclient.Artifact) {
	if !art.WasRedacted() {
		return
	}
	what := "values masked"
	if r := art.Redactions; r != nil && r.Count > 0 {
		noun := "values"
		if r.Count == 1 {
			noun = "value"
		}
		what = fmt.Sprintf("%d %s masked", r.Count, noun)
		if len(r.Rules) > 0 {
			rules := make([]string, 0, len(r.Rules))
			for _, rule := range slices.Sorted(maps.Keys(r.Rules)) {
				rules = append(rules, fmt.Sprintf("%s ×%d", rule, r.Rules[rule]))
			}
			what += " (" + strings.Join(rules, ", ") + ")"
		}
	}
	fmt.Fprintf(errOut, "cairn: warning: %s; stored content differs from input\n", what)
}
