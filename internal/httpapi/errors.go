// Package httpapi is the /v1 REST/JSON adapter over the core store service. It
// is a thin transport shim: every handler validates input, calls one core
// method, and renders the result or the single structured error envelope of
// ADR-0012. The web and MCP surfaces are peer adapters over the same core.
//
// Governing: ADR-0012 (Backend Platform and API Shape), ADR-0003 (one core,
// thin adapters), SPEC-0002 (Artifact Lifecycle, REST Endpoints, Security).
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/stump-wtf/cairn/internal/errs"
)

// errorEnvelope is the single non-2xx response shape (ADR-0012).
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// Violations is filled only for validation_failed and payload_too_large
// (SPEC-0019 VE-1, VE-5); every other code encodes exactly as it did before
// the field existed.
type errorBody struct {
	Code       errs.Code         `json:"code"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details,omitempty"`
	Violations []errs.Violation  `json:"violations,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
}

// statusFor maps a stable machine code to its aligned HTTP status.
func statusFor(code errs.Code) int {
	switch code {
	case errs.CodeNotFound:
		return http.StatusNotFound
	case errs.CodeUnauthorized:
		return http.StatusUnauthorized
	case errs.CodeForbidden:
		return http.StatusForbidden
	case errs.CodeValidation:
		return http.StatusBadRequest
	case errs.CodeConflict:
		return http.StatusConflict
	case errs.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case errs.CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// messageFor returns a stable, leak-free message per code. Uniform not-found is
// preserved: unknown, unauthorized, and expired ids all render identically.
func messageFor(code errs.Code) string {
	switch code {
	case errs.CodeNotFound:
		return "not found or expired"
	case errs.CodeUnauthorized:
		return "authentication required"
	case errs.CodeForbidden:
		return "forbidden"
	case errs.CodeValidation:
		return "the request was invalid"
	case errs.CodeConflict:
		return "conflict"
	case errs.CodePayloadTooLarge:
		return "payload too large"
	case errs.CodeRateLimited:
		return "rate limit exceeded"
	default:
		return "internal error"
	}
}

// violationsFor returns the violations a validation_failed or
// payload_too_large response carries, and its top-level message. A bare
// validation error (a site not yet migrated) renders as the single generic
// violation (SPEC-0019 VE-6); a bare payload-too-large names the body.
// Violations come from the domain error, never from parsing its string.
func violationsFor(code errs.Code, err error) ([]errs.Violation, string) {
	vs := errs.ViolationsOf(err)
	if len(vs) == 0 {
		if code == errs.CodePayloadTooLarge {
			vs = []errs.Violation{errs.NewViolation("body", errs.LocBody, errs.ReasonTooLarge)}
		} else {
			vs = []errs.Violation{errs.Generic()}
		}
	}
	return vs, errs.Summary(vs)
}

// writeError renders err as the ADR-0012 envelope with an aligned status and a
// request_id, logging the underlying error server-side with structured context.
// details carries only caller-supplied identifiers (e.g. the id in the path),
// never internal state, so the message stays uniform and leak-free.
//
// A validation_failed or payload_too_large envelope also carries the
// violations, and its message is built from them; details is never used to
// mirror violation fields. The log line keeps the full internal error either
// way.
//
// Governing: ADR-0012, ADR-0025, SPEC-0019 VE-1, VE-4, VE-5, VE-6
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, details map[string]string) {
	code := errs.CodeOf(err)
	status := statusFor(code)
	reqID := middleware.GetReqID(r.Context())

	logAttrs := []any{"code", code, "status", status, "method", r.Method, "path", r.URL.Path, "request_id", reqID, "error", err}
	if status >= 500 {
		s.log.ErrorContext(r.Context(), "httpapi: request failed", logAttrs...)
	} else {
		s.log.InfoContext(r.Context(), "httpapi: request rejected", logAttrs...)
	}

	body := errorBody{
		Code:      code,
		Message:   messageFor(code),
		Details:   details,
		RequestID: reqID,
	}
	if code == errs.CodeValidation || code == errs.CodePayloadTooLarge {
		body.Violations, body.Message = violationsFor(code, err)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: body})
}

// writeJSON renders v as a JSON response with the given status. encoding/json
// escapes HTML by default, so user-supplied strings cannot break out of the
// JSON context (output encoding).
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("httpapi: encode response", "error", err)
	}
}
