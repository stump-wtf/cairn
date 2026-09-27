package redact

// Rejection
//
// The error a refused field returns. It wraps ErrSecretDetected or
// ErrTooLargeToScan together with errs.ErrValidation, so errs.CodeOf renders
// validation_failed without the transport knowing about gitleaks. The typed
// SPEC-0019 violation is built from it by the wiring stories.
//
// Its message names the field, rule, line and column, and never the value or
// the line containing it (SPEC-0017 RD-11): Finding carries no such text, so
// there is nothing here that could.
//
// Governing: ADR-0023, SPEC-0017 RD-11, REQ "Error Handling Standards"
//
// @joestump 09/23/2026 - Added for cairn#289.

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/cairn/internal/errs"
)

// maxListed bounds how many findings a rejection message spells out.
const maxListed = 5

// Rejection is a refused field.
type Rejection struct {
	Field    string
	Reason   string    // ReasonSecretDetected or ReasonTooLargeToScan
	Findings []Finding // for ReasonSecretDetected
	Limit    int64     // for ReasonTooLargeToScan: the cap, in bytes
	Size     int64     // for ReasonTooLargeToScan: the field's size, in bytes
	// Unmaskable is set when mask mode could not mask every finding, so the
	// field is refused rather than stored with a value in it.
	Unmaskable bool
}

func (r *Rejection) Error() string {
	field := r.Field
	if field == "" {
		field = "content"
	}
	if r.Reason == ReasonTooLargeToScan {
		return fmt.Sprintf("%s: %d bytes is too large to scan for credentials (limit %d bytes)", field, r.Size, r.Limit)
	}
	var locs []string
	for i, f := range r.Findings {
		if i == maxListed {
			locs = append(locs, fmt.Sprintf("and %d more", len(r.Findings)-maxListed))
			break
		}
		loc := "rule " + f.Rule
		if f.Line > 0 {
			loc += fmt.Sprintf(", line %d", f.Line)
		}
		if f.Column > 0 {
			loc += fmt.Sprintf(", column %d", f.Column)
		}
		locs = append(locs, loc)
	}
	detail := strings.Join(locs, "; ")
	if r.Unmaskable {
		return fmt.Sprintf("%s: a credential was detected that could not be masked (%s); remove it", field, detail)
	}
	return fmt.Sprintf("%s: a credential was detected (%s); remove it or resend with --redact=mask", field, detail)
}

// Unwrap exposes the reason's sentinel and errs.ErrValidation.
func (r *Rejection) Unwrap() []error {
	if r.Reason == ReasonTooLargeToScan {
		return []error{ErrTooLargeToScan, errs.ErrValidation}
	}
	return []error{ErrSecretDetected, errs.ErrValidation}
}
