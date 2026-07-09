// Package errs defines Cairn's domain error taxonomy.
//
// Callers distinguish failures with sentinel/domain errors and wrap them with
// context on the way up preserving the chain with %w, so a transport adapter
// maps a domain error to a stable machine Code without string matching, then
// renders the single structured error envelope of ADR-0012.
//
// Governing: ADR-0012 (Backend Platform and API Shape),
// SPEC-0002 REQ "Error Handling Standards"
package errs

import (
	"errors"
	"fmt"
)

// Code is the stable, machine-consumable error code aligned with an HTTP
// status by the transport adapter. The CLI and agents branch on it.
type Code string

const (
	CodeNotFound        Code = "not_found"
	CodeUnauthorized    Code = "unauthorized"
	CodeForbidden       Code = "forbidden"
	CodeValidation      Code = "validation_failed"
	CodeConflict        Code = "conflict"
	CodePayloadTooLarge Code = "payload_too_large"
	CodeRateLimited     Code = "rate_limited"
	CodeInternal        Code = "internal"
)

// Error is a domain error carrying a stable Code and a human-readable message.
// Sentinels below are *Error values; wrapping one with fmt.Errorf("...: %w", e)
// keeps it discoverable via errors.Is / errors.As and CodeOf.
type Error struct {
	code Code
	msg  string
}

func (e *Error) Error() string { return e.msg }

// Code returns the stable machine code for this domain error.
func (e *Error) Code() Code { return e.code }

// New builds an ad-hoc domain error with a stable code.
func New(code Code, msg string) *Error { return &Error{code: code, msg: msg} }

// Sentinel domain errors for failures callers distinguish.
var (
	ErrNotFound         = New(CodeNotFound, "not found")
	ErrUnauthorized     = New(CodeUnauthorized, "unauthorized")
	ErrForbidden        = New(CodeForbidden, "forbidden")
	ErrValidation       = New(CodeValidation, "validation failed")
	ErrConflict         = New(CodeConflict, "conflict")
	ErrChecksumMismatch = New(CodeValidation, "body checksum does not match")
	ErrTooLarge         = New(CodePayloadTooLarge, "payload too large")
)

// CodeOf walks the error chain and returns the stable Code of the first domain
// Error found, or CodeInternal for an unclassified error. Handlers use this to
// map any error to an envelope without string matching.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var de *Error
	if errors.As(err, &de) {
		return de.code
	}
	return CodeInternal
}

// Validationf constructs a validation-coded error with a formatted message,
// still discoverable as a validation failure via CodeOf and errors.Is.
func Validationf(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrValidation)
}
