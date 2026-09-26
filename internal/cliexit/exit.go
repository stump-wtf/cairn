// Package cliexit implements the cairn CLI's stable exit-code taxonomy
// (SPEC-0008 "Machine-Readable Error Mapping and Exit Codes"). It maps any
// error the command layer can produce — a client-side usage error, a
// transport failure, or a decoded *cliclient.APIError — onto one of the
// fixed exit codes below, so scripts can branch on $? deterministically.
//
// Governing: SPEC-0008 (The cairn Command-Line Interface).
package cliexit

import (
	"context"
	"errors"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// Code is one of the fixed exit codes from SPEC-0008's table.
type Code int

// The stable exit-code taxonomy. Values and meanings are normative — do not
// renumber; scripts grep/compare on them.
const (
	Success          Code = 0   // 2xx
	Internal         Code = 1   // internal/unexpected, unclassified
	Usage            Code = 2   // bad flags/args, empty input, validation_failed
	NotAuthenticated Code = 3   // unauthorized (not authenticated / refresh failed)
	Forbidden        Code = 4   // forbidden
	NotFound         Code = 5   // not_found (or expired)
	Oversize         Code = 6   // payload_too_large
	RateLimited      Code = 7   // rate_limited
	Network          Code = 8   // transport error: DNS/TLS/connection
	Conflict         Code = 9   // conflict
	Interrupted      Code = 130 // SIGINT
)

// ErrUsage is the sentinel for client-side usage errors: bad flags/args,
// empty input, a malformed config value — anything the CLI rejects before
// making a network call. Wrap it with fmt.Errorf("...: %w", ErrUsage).
var ErrUsage = errors.New("cairn: usage error")

// ErrInterrupted is the sentinel command handlers return (or main
// synthesizes from a canceled SIGINT context) to signal Code Interrupted.
var ErrInterrupted = errors.New("cairn: interrupted")

// ForError maps err to its exit Code by walking the error chain: usage and
// interrupt sentinels first, then a transport failure (cliclient.ErrNetwork),
// then a decoded *cliclient.APIError's stable machine code, and finally
// Internal for anything unclassified. Callers must check for nil (Success)
// themselves — ForError(nil) also returns Success for convenience.
func ForError(err error) Code {
	if err == nil {
		return Success
	}
	switch {
	case errors.Is(err, ErrInterrupted), errors.Is(err, context.Canceled):
		return Interrupted
	case errors.Is(err, ErrUsage):
		return Usage
	case errors.Is(err, cliclient.ErrNetwork):
		return Network
	}

	var apiErr *cliclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case cliclient.CodeUnauthorized:
			return NotAuthenticated
		case cliclient.CodeForbidden:
			return Forbidden
		case cliclient.CodeNotFound:
			return NotFound
		case cliclient.CodePayloadTooLarge:
			return Oversize
		case cliclient.CodeRateLimited:
			return RateLimited
		case cliclient.CodeConflict:
			return Conflict
		case cliclient.CodeValidation:
			return Usage
		default:
			return Internal
		}
	}
	return Internal
}
