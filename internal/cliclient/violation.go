package cliclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Violation is one entry of an error envelope's `violations` array: the
// field the server rejected, where it was carried, why, and the limit it
// broke. It is the CLI's own copy of the wire shape — the server's
// internal/errs type is off limits to this package (ADR-0003) — so it
// carries only what the CLI renders.
//
// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-1, VE-8
type Violation struct {
	Field    string  `json:"field"`
	Location string  `json:"location"`
	Reason   string  `json:"reason"`
	Limit    *Scalar `json:"limit,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	Value    *string `json:"value,omitempty"`
	Message  string  `json:"message"`
	// Rule, Line and Column locate a secret_detected rejection (SPEC-0019
	// VE-2). The detected value itself is never sent (VE-3).
	Rule   string  `json:"rule,omitempty"`
	Line   *Scalar `json:"line,omitempty"`
	Column *Scalar `json:"column,omitempty"`
}

// The reasons the CLI words itself (SPEC-0019 VE-2). Any other reason is
// shown with the server's own message.
const (
	ReasonExceedsMax = "exceeds_max"
	ReasonTooLong    = "too_long"
	ReasonTooMany    = "too_many"
	ReasonTooLarge   = "too_large"
	ReasonInvalid    = "invalid"
)

// UnitSeconds and UnitBytes are the limit units the CLI reformats.
const (
	UnitSeconds = "seconds"
	UnitBytes   = "bytes"
)

// Scalar is a JSON number or string, kept as its text. A limit may be either
// (SPEC-0019 VE-1), and the CLI must not fail on the one it did not expect.
type Scalar struct {
	Text   string
	Number bool
}

// UnmarshalJSON accepts a JSON string or number; anything else is an error.
func (s *Scalar) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var t string
		if err := json.Unmarshal(b, &t); err != nil {
			return err
		}
		*s = Scalar{Text: t}
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("cliclient: violation value %s is neither a number nor a string", b)
	}
	*s = Scalar{Text: n.String(), Number: true}
	return nil
}

// MarshalJSON re-emits the scalar in the form it arrived in, so `--json`
// passes the server's violation through unchanged.
func (s Scalar) MarshalJSON() ([]byte, error) {
	if s.Number {
		return []byte(s.Text), nil
	}
	return json.Marshal(s.Text)
}

// Int reports the scalar as a whole number, whichever form it arrived in.
func (s *Scalar) Int() (int64, bool) {
	if s == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(s.Text, 10, 64)
	return n, err == nil
}

// String is the scalar's text, or "" when it is absent.
func (s *Scalar) String() string {
	if s == nil {
		return ""
	}
	return s.Text
}

// decodeViolations decodes the envelope's raw `violations` array. It is all
// or nothing: when any entry fails to decode, it returns nil, so the caller
// shows the top-level message (which summarises every violation) rather than
// a partial list that reads as complete.
func decodeViolations(raw json.RawMessage) []Violation {
	if len(raw) == 0 {
		return nil
	}
	var vs []Violation
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) == 0 {
		return nil
	}
	return vs
}
