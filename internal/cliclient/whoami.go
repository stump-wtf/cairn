package cliclient

import (
	"context"
	"net/http"
)

// Whoami is the CLI's view of GET /v1/whoami (internal/httpapi/auth.go
// handleAPIWhoami) — the bearer-token identity round trip `cairn login`
// verifies a candidate token against and `cairn whoami` re-verifies on every
// invocation (cairn#21, SPEC-0008 "Authentication and Session Lifecycle").
type Whoami struct {
	ActorID       string `json:"actor_id"`
	Channel       string `json:"channel"`
	Authenticated bool   `json:"authenticated"`
}

// Whoami calls GET /v1/whoami with the Client's configured bearer token and
// decodes the identity it resolves to. An empty/unknown/revoked token comes
// back as a decoded *APIError with CodeUnauthorized (ErrNotAuthenticated),
// exactly like any other endpoint's 401 — callers branch on that sentinel,
// never on a bespoke "login failed" shape (SPEC-0008 "Sentinel error drives
// control flow").
func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/v1/whoami", nil, "")
	if err != nil {
		return nil, err
	}
	var who Whoami
	if err := DecodeInto(resp, &who); err != nil {
		return nil, err
	}
	return &who, nil
}
