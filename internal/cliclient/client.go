// Package cliclient is the cairn CLI's typed HTTP client over the core /v1
// REST API. Per ADR-0003 the CLI is a pure network REST client with no
// domain logic — this package therefore never imports the server's core
// packages (internal/store, internal/artifact, internal/errs); it defines
// its own wire-level Code enum and decodes the ADR-0012 structured error
// envelope into typed sentinel errors the command layer branches on
// (SPEC-0008 "Machine-Readable Error Mapping and Exit Codes", "Error
// Handling Standards").
//
// Governing: SPEC-0008 (The cairn Command-Line Interface), ADR-0003 (Triple
// Surface Parity), ADR-0012 (Backend Platform and API Shape, error
// contract).
package cliclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Code is the stable, machine-consumable error code the server sends in its
// ADR-0012 envelope. It is intentionally a distinct type from the server's
// internal/errs.Code — the CLI must not import server-internal packages.
type Code string

// The stable enum aligned with ADR-0012's error contract.
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

// ErrNetwork is a transport-level sentinel: DNS/TLS/connection failures that
// never reached the server, distinct from an application-level error code
// (SPEC-0008 "Unreachable server is distinct from an HTTP error"). Wrap it
// with fmt.Errorf("...: %w: %w", ErrNetwork, cause).
var ErrNetwork = errors.New("cairn: network error")

// APIError is a decoded ADR-0012 error envelope. It is also used as a
// sentinel: errors.Is compares only the Code field (see Is below), so a
// decoded *APIError with CodeUnauthorized matches the package-level
// ErrNotAuthenticated sentinel without pointer identity.
type APIError struct {
	Code       Code
	Message    string
	Details    map[string]string
	RequestID  string
	HTTPStatus int
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s (request_id=%s)", e.Message, e.RequestID)
	}
	return e.Message
}

// Is makes errors.Is(err, ErrNotAuthenticated) (and friends) match any
// decoded *APIError with the same Code, regardless of its message or
// request_id — the command layer branches on the stable code, never on
// prose (SPEC-0008 "Sentinel error drives control flow").
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	if !ok || e == nil || t == nil {
		return false
	}
	return e.Code == t.Code
}

// Sentinel domain errors the command layer branches on (SPEC-0008 "Error
// Handling Standards": "MUST define sentinel errors for the domain failures
// the CLI branches on (e.g. ErrNotAuthenticated, ErrExpired, ErrOversize,
// ErrConflict)"). ErrExpired aliases ErrNotFound: the server intentionally
// renders "not found" and "expired" identically (ADR-0012 uniform
// not-found messaging, internal/httpapi/errors.go messageFor) so a client
// can never distinguish "never existed" from "expired" — both are the same
// wire code. The alias keeps that concept nameable in CLI code without
// inventing a distinction the server does not make.
var (
	ErrNotAuthenticated = &APIError{Code: CodeUnauthorized, Message: "authentication required"}
	ErrForbidden        = &APIError{Code: CodeForbidden, Message: "forbidden"}
	ErrNotFound         = &APIError{Code: CodeNotFound, Message: "not found"}
	ErrExpired          = ErrNotFound
	ErrValidation       = &APIError{Code: CodeValidation, Message: "validation failed"}
	ErrConflict         = &APIError{Code: CodeConflict, Message: "conflict"}
	ErrOversize         = &APIError{Code: CodePayloadTooLarge, Message: "payload too large"}
	ErrRateLimited      = &APIError{Code: CodeRateLimited, Message: "rate limited"}
	ErrServerInternal   = &APIError{Code: CodeInternal, Message: "internal error"}
)

// envelope mirrors the server's ADR-0012 wire shape
// (internal/httpapi/errors.go errorEnvelope/errorBody).
type envelope struct {
	Error struct {
		Code      Code              `json:"code"`
		Message   string            `json:"message"`
		Details   map[string]string `json:"details,omitempty"`
		RequestID string            `json:"request_id,omitempty"`
	} `json:"error"`
}

// Client is a thin, typed HTTP client bound to one server. It carries no
// business logic: callers build request bodies and decode success payloads;
// Client only attaches auth, performs the round trip, and maps failures.
type Client struct {
	// BaseURL is the resolved API origin, e.g. https://cairn.stump.wtf (no
	// trailing slash, no /v1 suffix — request paths supply that).
	BaseURL string
	// Token is the bearer credential attached to every request
	// (SPEC-0008 "Every mutating command carries auth"). Empty means
	// unauthenticated; callers should check before calling Do so the CLI
	// fails locally with ErrNotAuthenticated instead of sending a
	// request the server will reject.
	Token string
	// UserAgent identifies the CLI build to the server for diagnostics.
	UserAgent string

	httpClient *http.Client
}

// New builds a Client for baseURL with the given bearer token (may be
// empty). baseURL must already be validated (see internal/cliconfig).
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		UserAgent: "cairn-cli",
		httpClient: &http.Client{
			// A generous ceiling, not a per-request expectation: callers
			// that need finer-grained cancellation (bounded uploads,
			// Ctrl-C) supply a context with their own deadline.
			Timeout: 5 * time.Minute,
		},
	}
}

// HTTPClient exposes the underlying *http.Client so tests and future
// callers (e.g. a bounded uploader) can override transport/timeout.
func (c *Client) HTTPClient() *http.Client {
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return c.httpClient
}

// SetHTTPClient overrides the transport, primarily for tests.
func (c *Client) SetHTTPClient(h *http.Client) { c.httpClient = h }

// newRequest builds an authenticated *http.Request against path (e.g.
// "/v1/artifacts") without sending it, so callers can attach extra headers
// (SPEC-0008 client-known hints like X-Cairn-Title) before the round trip.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("cliclient: no API base URL configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("cliclient: build request %s %s: %w", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// send performs the round trip, wrapping a transport failure (the request
// never reached the server: DNS/TLS/connection) in ErrNetwork so the command
// layer maps it to the distinct "unreachable" exit code rather than an
// application-level error code (SPEC-0008 "Unreachable server is distinct
// from an HTTP error").
func (c *Client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cliclient: %s %s: %w: %w", req.Method, req.URL.Path, ErrNetwork, err)
	}
	return resp, nil
}

// Do issues an authenticated request against path (e.g. "/v1/artifacts")
// and returns the raw response for the caller to decode. A transport
// failure (never reached the server) is wrapped in ErrNetwork; the caller
// is responsible for checking resp.StatusCode and calling DecodeError for
// non-2xx responses.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, body, contentType)
	if err != nil {
		return nil, err
	}
	return c.send(req)
}

// DecodeError reads and closes resp.Body and, for a non-2xx response,
// decodes the ADR-0012 error envelope into an *APIError. It returns nil for
// a 2xx response (the body is still consumed/closed so the connection can
// be reused). Callers that need the success body must read it before status
// codes below 300 would otherwise be discarded by DecodeError — use
// DecodeInto instead for typed success payloads.
func DecodeError(resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return &APIError{
			Code:       CodeInternal,
			Message:    fmt.Sprintf("unexpected response (status %d)", resp.StatusCode),
			HTTPStatus: resp.StatusCode,
		}
	}
	return &APIError{
		Code:       env.Error.Code,
		Message:    env.Error.Message,
		Details:    env.Error.Details,
		RequestID:  env.Error.RequestID,
		HTTPStatus: resp.StatusCode,
	}
}

// DecodeInto reads and closes resp.Body, decoding a 2xx JSON body into v or
// a non-2xx body into a *APIError via DecodeError.
func DecodeInto(resp *http.Response, v any) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DecodeError(resp)
	}
	defer resp.Body.Close()
	if v == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("cliclient: decode response body: %w", err)
	}
	return nil
}
