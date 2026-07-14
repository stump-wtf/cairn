// Package webhook is the core service for the webhook (requestbin) share
// type: it provisions a capture endpoint as an ordinary Cairn artifact, holds
// its captured requests in a bounded, seq-ordered ring buffer — metadata in
// PostgreSQL, oversized bodies spilled to content-addressed object storage —
// and evicts the oldest record whenever a capture would push the buffer past
// its cap. It is deliberately the MODEL and MANAGEMENT layer only: this story
// does not open the public, anonymous-write ingress that fills the buffer
// (that lands in a follow-up story per issue #83); Capture is the core method
// the eventual ingress handler will call, exercised directly by integration
// tests here to simulate captures.
//
// The REST (and later MCP) surfaces are thin adapters over this one package
// (ADR-0003), exactly mirroring internal/trajectory's split for the other live
// share type.
//
// Governing: ADR-0010 (Live Webhook Endpoints and Real-Time Stream Capture),
// ADR-0008 (Storage & Content Model), SPEC-0005 (Webhook Inspector).
package webhook

import (
	"time"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// DefaultRequestCap is the per-endpoint ring-buffer size: at most the last N
// captured requests are retained; overflow evicts the oldest (SPEC-0005
// "Ring-Buffer Retention and Caps", e.g. N = 500).
const DefaultRequestCap = 500

// DefaultResponseStatus is the fixed, benign status the (future) ingress
// records for every capture unless the endpoint is configured otherwise — data
// Cairn returns, never behavior an inbound payload can steer (SPEC-0005 "Fixed
// Benign Response").
const DefaultResponseStatus = 200

// Sentinel domain errors callers distinguish, each mapped to a stable code by
// a transport adapter via errs.CodeOf without string matching (SPEC-0005
// "Error Handling Standards").
var (
	// ErrEndpointNotFound is returned uniformly for an unknown, unauthorized, or
	// expired endpoint id so probing leaks no signal (ADR-0007 link-capability,
	// SPEC-0005 "Unguessable ID & No Enumeration").
	ErrEndpointNotFound = errs.New(errs.CodeNotFound, "webhook endpoint not found")
	// ErrRequestNotFound is returned uniformly for an unknown or evicted
	// captured-request seq.
	ErrRequestNotFound = errs.New(errs.CodeNotFound, "captured request not found")
)

// EndpointInput is the input to CreateEndpoint. Provenance, Access, and
// ExpiresAt are the ordinary artifact envelope a webhook endpoint gets for
// free (SPEC-0002); RequestCap defaults to DefaultRequestCap when zero.
type EndpointInput struct {
	Title      string
	RequestCap int
	Provenance artifact.Provenance
	Access     artifact.AccessPolicy
	ExpiresAt  time.Time
}

func (in EndpointInput) validate() error {
	switch {
	case in.Provenance.Channel == "":
		return errs.Validationf("webhook: provenance channel is required")
	case in.Provenance.ActorID == "":
		return errs.Validationf("webhook: provenance actor is required")
	case in.Provenance.CapturedAt.IsZero():
		return errs.Validationf("webhook: provenance capture time is required")
	case in.Access.OwnerID == "":
		return errs.Validationf("webhook: access owner is required")
	case in.Access.Visibility == "":
		return errs.Validationf("webhook: access visibility is required")
	case in.ExpiresAt.IsZero():
		return errs.Validationf("webhook: expiry is required")
	case in.RequestCap < 0:
		return errs.Validationf("webhook: request cap must not be negative")
	}
	return nil
}

// Endpoint is a webhook endpoint: its artifact envelope plus the ring-buffer
// cap the owner configured (or the default).
type Endpoint struct {
	PublicID   string
	Title      string
	RequestCap int
	Provenance artifact.Provenance
	Access     artifact.AccessPolicy
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// BodyRef references a captured request's oversized body stored as a
// content-addressed blob (SPEC-0005 "Body stored verbatim and
// content-addressed"). It is fetched lazily on expand.
type BodyRef struct {
	SHA256    string
	Size      int64
	Truncated bool
}

// CaptureInput is one inbound request as the (future) ingress will present it
// to Capture. Headers is a sanitized-on-write multi-map (SPEC-0005 "Header
// Hygiene & Ephemerality as Containment"); Status is the fixed response Cairn
// decided to return, recorded as data rather than derived from the payload
// (SPEC-0005 "Fixed Benign Response").
type CaptureInput struct {
	Method      string
	Path        string
	Query       string
	Headers     map[string][]string
	ContentType string
	Body        []byte
	Status      int
}

func (in CaptureInput) validate() error {
	if in.Method == "" {
		return errs.Validationf("webhook: captured request method is required")
	}
	if in.Status < 100 || in.Status > 599 {
		return errs.Validationf("webhook: captured response status must be a valid HTTP status")
	}
	return nil
}

// Request is one captured request record: the seq-ordered, PostgreSQL-resident
// metadata the inspector queries for the status mix and method counts without
// ever reading a body (SPEC-0005 "Metadata queryable without reading the
// body"), plus its body disposition. Seq is the request's stable identity —
// monotonic per endpoint, never reused even across ring-buffer eviction — and
// is the webhook_request anchor target (SPEC-0005).
type Request struct {
	Seq         int64
	ReceivedAt  time.Time
	Method      string
	Path        string
	Query       string
	Headers     map[string][]string
	Status      int
	ContentType string
	BodySize    int64
	// Exactly one of Inline / Ref describes the body (both zero => empty body).
	Inline []byte
	Ref    *BodyRef
}
