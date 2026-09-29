package redact

import (
	"fmt"
	"maps"
	"regexp"
)

// Recorded Scan Outcome
//
// Summary is the part of an Outcome that Cairn stores and shows the owner: a
// status, a count and counts per rule ID (SPEC-0017 RD-9). It has no Findings,
// so it carries no line or column either, and no field of it can hold a secret,
// a hash of one or the matched line. TestSummaryHoldsNoSecret pins that shape.
//
// The zero Summary is StatusUnscanned. That is deliberate: every insert path
// writes a Summary, and a path the scanner has not been wired into yet writes
// the zero value, so it records "unscanned" instead of claiming a scan that never
// ran. Only Outcome.Summary, called on a real scan result, produces "clean" or
// better.
//
// Governing: ADR-0023, SPEC-0017 RD-9
//
// @joestump 09/25/2026 - Added for cairn#290. The write paths set it when
// #291, #292 and #293 wire the scanner in.
type Summary struct {
	Status Status
	Count  int
	Rules  map[string]int // rule ID -> count
}

// Statuses lists every recordable status. The migration's CHECK constraints
// allow exactly this set (TestRedactionStatusConstraintMatchesDomain).
var Statuses = []Status{
	StatusUnscanned,
	StatusClean,
	StatusMasked,
	StatusNotScannedBinary,
	StatusNotScannedOversize,
}

// ruleIDPattern bounds what may be recorded as a rule ID. gitleaks rule IDs are
// short kebab-case names from the embedded config ("github-pat",
// "curl-basic-auth"). The check is a guard against a caller recording
// something that is not a rule ID, such as a match or a line of the scanned
// text, in a column the owner is shown.
var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Summary drops the findings and copies the per-rule counts, so the result
// shares nothing with the scan that produced it.
func (o Outcome) Summary() Summary {
	return Summary{Status: o.Status, Count: o.Count, Rules: maps.Clone(o.Rules)}
}

// Redacted reports whether any value was masked. The create response's
// "redacted" flag is this (SPEC-0017 RD-9, RD-10).
func (s Summary) Redacted() bool { return s.Status == StatusMasked && s.Count > 0 }

// Normalized returns s as it is stored: an empty status becomes
// StatusUnscanned and a nil rules map becomes an empty one, so a JSONB column
// never holds null. It fails on a status outside Statuses, a negative count or
// a rule ID that does not look like one.
func (s Summary) Normalized() (Summary, error) {
	out := Summary{Status: s.Status, Count: s.Count, Rules: make(map[string]int, len(s.Rules))}
	if out.Status == "" {
		out.Status = StatusUnscanned
	}
	if severity(out.Status) < 0 {
		return Summary{}, fmt.Errorf("redact: unknown status %q", out.Status)
	}
	if out.Count < 0 {
		return Summary{}, fmt.Errorf("redact: negative redaction count %d", out.Count)
	}
	for rule, n := range s.Rules {
		// The rule ID is deliberately not quoted in the error: if a caller has
		// put scanned text here by mistake, the error must not repeat it.
		if !ruleIDPattern.MatchString(rule) {
			return Summary{}, fmt.Errorf("redact: a recorded rule ID is not a rule ID (%d bytes)", len(rule))
		}
		if n < 0 {
			return Summary{}, fmt.Errorf("redact: negative count for rule %s", rule)
		}
		out.Rules[rule] = n
	}
	return out, nil
}

// Merge combines the outcome already recorded on a row with a new one, for a
// row whose content grows after it is created (a run's appended spans). Counts
// and per-rule counts add, and the status is the less reassuring of the two:
// clean, then masked, then not_scanned_binary, then not_scanned_oversize, then
// unscanned. A row that ever held unscanned content therefore stays unscanned,
// because the new scan says nothing about what was stored before it.
//
// A zero next is "unscanned" on this side too, as it is everywhere else: an
// append path the scanner is not wired into must not leave a clean run
// reading clean. An unknown next status is carried through, so Normalized
// refuses the write instead of the merge silently dropping it.
func (s Summary) Merge(next Summary) Summary {
	out := Summary{Status: s.Status, Count: s.Count + next.Count, Rules: make(map[string]int, len(s.Rules)+len(next.Rules))}
	if out.Status == "" {
		out.Status = StatusUnscanned
	}
	n := next.Status
	if n == "" {
		n = StatusUnscanned
	}
	if severity(n) < 0 || severity(n) > severity(out.Status) {
		out.Status = n
	}
	for rule, n := range s.Rules {
		out.Rules[rule] += n
	}
	for rule, n := range next.Rules {
		out.Rules[rule] += n
	}
	return out
}

// severity orders statuses for Merge, and is -1 for an unknown status.
func severity(s Status) int {
	switch s {
	case StatusClean:
		return 0
	case StatusMasked:
		return 1
	case StatusNotScannedBinary:
		return 2
	case StatusNotScannedOversize:
		return 3
	case StatusUnscanned:
		return 4
	}
	return -1
}
