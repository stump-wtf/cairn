package errs

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A Violation names one thing wrong with a request: which caller-facing field,
// where it was sent, why it failed, and the limit it broke. Violations are
// built in the domain layer (never parsed out of an error string by a
// transport) and rendered identically on REST, MCP and the CLI.
//
// Governing: ADR-0025 (Actionable Validation Errors), SPEC-0019 VE-1, VE-2,
// VE-3, VE-6

// Location is where the offending field was carried on the request.
type Location string

const (
	LocHeader Location = "header"
	LocQuery  Location = "query"
	LocForm   Location = "form"
	LocBody   Location = "body"
	LocPath   Location = "path"
)

// Reason is a code from the closed registry of SPEC-0019 VE-2. A new reason is
// added here, to Reasons, and to the public error reference before any code
// emits it.
type Reason string

const (
	ReasonRequired       Reason = "required"
	ReasonInvalidFormat  Reason = "invalid_format"
	ReasonInvalidCharset Reason = "invalid_charset"
	ReasonUppercase      Reason = "uppercase"
	ReasonTooLong        Reason = "too_long"
	ReasonTooShort       Reason = "too_short"
	ReasonTooMany        Reason = "too_many"
	ReasonExceedsMax     Reason = "exceeds_max"
	ReasonNotPositive    Reason = "not_positive"
	ReasonUnknownValue   Reason = "unknown_value"
	ReasonDuplicate      Reason = "duplicate"
	ReasonMismatch       Reason = "mismatch"
	ReasonNotAllowed     Reason = "not_allowed"
	ReasonChecksum       Reason = "checksum_mismatch"
	ReasonTooLarge       Reason = "too_large"
	ReasonTooLargeToScan Reason = "too_large_to_scan"
	ReasonSecretDetected Reason = "secret_detected"
	// ReasonInvalid is reserved for validation sites not yet migrated to a
	// specific reason (VE-6). New code never emits it.
	ReasonInvalid Reason = "invalid"
)

// Reasons is the closed registry, in the order SPEC-0019 VE-2 lists it.
var Reasons = []Reason{
	ReasonRequired, ReasonInvalidFormat, ReasonInvalidCharset, ReasonUppercase,
	ReasonTooLong, ReasonTooShort, ReasonTooMany, ReasonExceedsMax,
	ReasonNotPositive, ReasonUnknownValue, ReasonDuplicate, ReasonMismatch,
	ReasonNotAllowed, ReasonChecksum, ReasonTooLarge, ReasonTooLargeToScan,
	ReasonSecretDetected, ReasonInvalid,
}

// Units a limit may be expressed in (VE-1).
const (
	UnitSeconds = "seconds"
	UnitBytes   = "bytes"
	UnitCount   = "count"
	UnitChars   = "chars"
)

// MaxValueBytes caps the echoed offending value (VE-3).
const MaxValueBytes = 64

// MaxViolations bounds one error's violation list, so a request carrying
// thousands of bad items cannot turn its own rejection into a large response.
const MaxViolations = 64

// The field and message a bare validation error renders as (VE-6).
const (
	FieldRequest   = "request"
	MessageInvalid = "the request was invalid"
)

// Violation is one entry of an error envelope's `violations` array (VE-1).
type Violation struct {
	Field    string   `json:"field"`
	Location Location `json:"location"`
	Reason   Reason   `json:"reason"`
	Limit    any      `json:"limit,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Value    *string  `json:"value,omitempty"`
	Message  string   `json:"message"`
	// Extra holds the reason-specific keys VE-2 documents (rule, line and
	// column for secret_detected). They are flattened into the object on
	// encode.
	Extra map[string]any `json:"-"`
}

// MarshalJSON flattens Extra beside the fixed keys. A fixed key always wins,
// and "value" is never taken from Extra, so Extra cannot overwrite the
// field, reason or message, nor smuggle in an echo VE-3 forbids.
func (v Violation) MarshalJSON() ([]byte, error) {
	type plain Violation
	base, err := json.Marshal(plain(v))
	if err != nil || len(v.Extra) == 0 {
		return base, err
	}
	var m map[string]any
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(v.Extra))
	for k := range v.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, fixed := m[k]; fixed || k == "value" {
			continue
		}
		m[k] = v.Extra[k]
	}
	return json.Marshal(m)
}

// Invalid is the domain error carrying one or more violations. It unwraps to
// ErrValidation (or to ErrTooLarge when every violation is too_large), so
// CodeOf and errors.Is(err, ErrValidation) behave exactly as before. It may
// also wrap a cause, such as a package sentinel, so existing errors.Is checks
// on that sentinel keep passing.
type Invalid struct {
	Violations []Violation
	cause      error
}

// Error is the internal string that is logged. It carries every violation's
// message; the response is shaped from Violations, never from this string.
func (e *Invalid) Error() string {
	s := Summary(e.Violations)
	if e.cause != nil {
		return s + ": " + e.cause.Error()
	}
	return s + ": " + e.sentinel().Error()
}

// Unwrap exposes the code-bearing sentinel first (CodeOf takes the first
// domain *Error in the chain), then any cause.
func (e *Invalid) Unwrap() []error {
	if e.cause != nil {
		return []error{e.sentinel(), e.cause}
	}
	return []error{e.sentinel()}
}

func (e *Invalid) sentinel() error {
	if len(e.Violations) == 0 {
		return ErrValidation
	}
	for _, v := range e.Violations {
		if v.Reason != ReasonTooLarge {
			return ErrValidation
		}
	}
	return ErrTooLarge
}

// Because records cause, so errors.Is(err, cause) holds, and returns e.
func (e *Invalid) Because(cause error) *Invalid {
	e.cause = cause
	return e
}

// Opt adjusts a violation under construction.
type Opt func(*Violation, *msgHints)

type msgHints struct {
	expect string
}

// WithLimit records the limit that was broken, and its unit.
func WithLimit(limit any, unit string) Opt {
	return func(v *Violation, _ *msgHints) {
		v.Limit = limit
		v.Unit = unit
	}
}

// WithValue echoes the offending input, truncated to MaxValueBytes on a UTF-8
// boundary. Never pass body content, a credential-bearing header or a detected
// secret (VE-3); NewViolation drops the value for secret_detected regardless.
func WithValue(s string) Opt {
	return func(v *Violation, _ *msgHints) {
		t := truncateUTF8(s, MaxValueBytes)
		v.Value = &t
	}
}

// WithExpect describes what a valid value looks like ("a positive integer
// number of seconds"). It only shapes the message.
func WithExpect(desc string) Opt {
	return func(_ *Violation, h *msgHints) { h.expect = desc }
}

// WithExtra adds a reason-specific key (VE-2), such as rule, line or column.
func WithExtra(key string, val any) Opt {
	return func(v *Violation, _ *msgHints) {
		if v.Extra == nil {
			v.Extra = map[string]any{}
		}
		v.Extra[key] = val
	}
}

// NewViolation builds one violation. Its message comes from message(), the
// single owner of every sentence.
func NewViolation(field string, loc Location, r Reason, opts ...Opt) Violation {
	v := Violation{Field: field, Location: loc, Reason: r}
	var h msgHints
	for _, o := range opts {
		o(&v, &h)
	}
	if r == ReasonSecretDetected {
		v.Value = nil
	}
	v.Message = message(v, h)
	return v
}

// Violate builds a single-violation domain error.
func Violate(field string, loc Location, r Reason, opts ...Opt) *Invalid {
	return &Invalid{Violations: []Violation{NewViolation(field, loc, r, opts...)}}
}

// Join merges the violations of several errors into one, for validators that
// report every failing element (VE-4). Nil entries are skipped, and the result
// is nil when nothing was joined. The first cause seen is kept. The list is
// bounded by MaxViolations.
func Join(all ...*Invalid) *Invalid {
	var out *Invalid
	for _, e := range all {
		if e == nil || len(e.Violations) == 0 {
			continue
		}
		if out == nil {
			out = &Invalid{}
		}
		if out.cause == nil {
			out.cause = e.cause
		}
		for _, v := range e.Violations {
			if len(out.Violations) == MaxViolations {
				return out
			}
			out.Violations = append(out.Violations, v)
		}
	}
	return out
}

// JoinErrors merges the violations of several validator results, so one
// response can name, say, a bad TTL and a bad tag together. Nil errors are
// skipped. An error that carries no violations is returned as it is, since it
// cannot be merged; otherwise the result is the joined *Invalid, or nil.
func JoinErrors(all ...error) error {
	var parts []*Invalid
	for _, err := range all {
		if err == nil {
			continue
		}
		var inv *Invalid
		if !errors.As(err, &inv) {
			return err
		}
		parts = append(parts, inv)
	}
	if out := Join(parts...); out != nil {
		return out
	}
	return nil
}

// ViolationsOf returns the violations carried anywhere in err's chain, or nil.
func ViolationsOf(err error) []Violation {
	var inv *Invalid
	if errors.As(err, &inv) {
		return inv.Violations
	}
	return nil
}

// Generic is the single violation a bare validation error renders as (VE-6).
func Generic() Violation {
	return Violation{Field: FieldRequest, Location: LocBody, Reason: ReasonInvalid, Message: MessageInvalid}
}

// Summary is the top-level message for a violation list (VE-4): the violation's
// "field: message" when there is one, and "N problems: a; b; …" when there are
// several. The generic violation renders as the bare generic message.
func Summary(vs []Violation) string {
	switch len(vs) {
	case 0:
		return MessageInvalid
	case 1:
		return summaryLine(vs[0])
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = summaryLine(v)
	}
	return fmt.Sprintf("%d problems: %s", len(vs), strings.Join(parts, "; "))
}

func summaryLine(v Violation) string {
	if v.Reason == ReasonInvalid && v.Field == FieldRequest {
		return v.Message
	}
	return v.Field + ": " + v.Message
}

// message owns every sentence a violation can carry, so the wording is
// consistent across every site and tested once. The field name is not part of
// the sentence: Summary and the CLI prefix it.
func message(v Violation, h msgHints) string {
	subject := ""
	if v.Value != nil {
		subject = strconv.Quote(*v.Value) + " "
	}
	limit := limitText(v.Limit, v.Unit)
	switch v.Reason {
	case ReasonRequired:
		return "is required"
	case ReasonInvalidFormat:
		if h.expect != "" {
			return subject + "is not valid: it must be " + h.expect
		}
		return subject + "is not in a valid format"
	case ReasonInvalidCharset:
		if h.expect != "" {
			return subject + "contains a character that is not allowed: use only " + h.expect
		}
		return subject + "contains a character that is not allowed"
	case ReasonUppercase:
		return subject + "must be lowercase"
	case ReasonTooLong:
		return subject + "is too long: the maximum is " + limit
	case ReasonTooShort:
		return subject + "is too short: the minimum is " + limit
	case ReasonTooMany:
		return "has too many entries: the maximum is " + limit
	case ReasonExceedsMax:
		return subject + "exceeds the maximum of " + limit
	case ReasonNotPositive:
		if h.expect != "" {
			return subject + "is not valid: it must be " + h.expect
		}
		return subject + "must be positive"
	case ReasonUnknownValue:
		if limit != "" {
			return subject + "is not a recognised value: expected " + limit
		}
		return subject + "is not a recognised value"
	case ReasonDuplicate:
		return subject + "is a duplicate"
	case ReasonMismatch:
		if h.expect != "" {
			return subject + "does not match " + h.expect
		}
		return subject + "does not match"
	case ReasonNotAllowed:
		if h.expect != "" {
			return subject + "is not allowed: " + h.expect
		}
		return subject + "is not allowed here"
	case ReasonChecksum:
		return "does not match the SHA-256 of the received body"
	case ReasonTooLarge:
		if limit != "" {
			return "is too large: the maximum is " + limit
		}
		return "is too large"
	case ReasonTooLargeToScan:
		return "is too large to scan for credentials: the maximum is " + limit
	case ReasonSecretDetected:
		var at []string
		if r, ok := v.Extra["rule"]; ok {
			at = append(at, fmt.Sprintf("rule %v", r))
		}
		if l, ok := v.Extra["line"]; ok {
			at = append(at, fmt.Sprintf("line %v", l))
		}
		where := ""
		if len(at) > 0 {
			where = " (" + strings.Join(at, ", ") + ")"
		}
		return "a credential was detected" + where + "; remove it or resend with --redact=mask"
	default:
		return MessageInvalid
	}
}

func limitText(limit any, unit string) string {
	if limit == nil {
		return ""
	}
	s := fmt.Sprint(limit)
	if unit == "" || unit == UnitCount {
		return s
	}
	return s + " " + unit
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune. Only the
// cut point is examined: it backs off to the start of the rune straddling it,
// by at most UTFMax-1 bytes. An invalid byte earlier in s is kept (encoding/json
// replaces it with U+FFFD) rather than emptying the whole echo, which trimming
// until the prefix validated would do.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := 1; i < utf8.UTFMax && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return s[:n]
}
