package clicmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliexit"
)

// usageErrorf builds a client-side usage error (bad flags/args, empty
// input) wrapping cliexit.ErrUsage so it maps to exit code 2.
func usageErrorf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, cliexit.ErrUsage)...)
}

// jsonErrorEnvelope is the JSON shape written to stderr on failure in
// --json mode (SPEC-0008 "JSON error payload"): either the server's decoded
// ADR-0012 envelope, or an equivalent object for a client-side/transport
// failure that never reached the server.
type jsonErrorEnvelope struct {
	Error jsonErrorBody `json:"error"`
}

type jsonErrorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Details   map[string]string `json:"details,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// printError writes err to errOut in the CLI's stable, greppable stderr
// format ("cairn: <tag>: <message>"), or as a JSON envelope when jsonOut is
// set (SPEC-0008 "Machine-Readable Error Mapping and Exit Codes",
// "JSON error payload"). It never writes to stdout, so a JSON-mode caller's
// stdout is never polluted with a partial success object.
func printError(errOut io.Writer, err error, jsonOut bool) {
	if err == nil {
		return
	}
	code := cliexit.ForError(err)

	var apiErr *cliclient.APIError
	hasAPIErr := errors.As(err, &apiErr)

	if jsonOut {
		body := jsonErrorBody{Message: err.Error()}
		if hasAPIErr {
			body.Code = string(apiErr.Code)
			body.Message = apiErr.Message
			body.Details = apiErr.Details
			body.RequestID = apiErr.RequestID
		} else {
			body.Code = jsonCodeFor(code)
		}
		_ = json.NewEncoder(errOut).Encode(jsonErrorEnvelope{Error: body})
		return
	}

	tag := tagFor(code)
	msg := err.Error()
	if hasAPIErr {
		msg = apiErr.Message
	}
	fmt.Fprintf(errOut, "cairn: %s: %s\n", tag, msg)
	if hasAPIErr && apiErr.RequestID != "" {
		fmt.Fprintf(errOut, "cairn: request_id=%s\n", apiErr.RequestID)
	}
}

// tagFor gives each exit code a short, stable stderr tag so scripts can
// grep "cairn: unauthorized:" etc. without depending on prose.
func tagFor(code cliexit.Code) string {
	switch code {
	case cliexit.Usage:
		return "usage"
	case cliexit.NotAuthenticated:
		return "unauthorized"
	case cliexit.Forbidden:
		return "forbidden"
	case cliexit.NotFound:
		return "not-found"
	case cliexit.Oversize:
		return "too-large"
	case cliexit.RateLimited:
		return "rate-limited"
	case cliexit.Network:
		return "network"
	case cliexit.Conflict:
		return "conflict"
	case cliexit.Interrupted:
		return "interrupted"
	default:
		return "error"
	}
}

func jsonCodeFor(code cliexit.Code) string {
	switch code {
	case cliexit.Usage:
		return "usage_error"
	case cliexit.Network:
		return "network_error"
	case cliexit.Interrupted:
		return "interrupted"
	default:
		return "internal"
	}
}
