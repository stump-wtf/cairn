// Package artifact defines the Cairn Artifact aggregate — the single shared
// unit and aggregate root — and its creation invariants.
//
// Every artifact carries a short opaque public id, a share type, a body
// reference (or, for bundles, a member manifest), metadata, provenance, an
// access policy, an expiry, and an annotation stream. At creation provenance,
// access policy, and expiry are all mandatory. New kinds (image, webhook,
// trajectory) are modeled as share types over this one aggregate, never as a
// competing top-level entity.
//
// Governing: ADR-0001 (Cairn as AI-Native Artifact Store),
// ADR-0007 (Provenance, Link Access, Default Expiry),
// SPEC-0002 REQ "Artifact Aggregate and Invariants"
package artifact

import (
	"time"

	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/redact"
)

// ShareType classifies an artifact and (via the registry in SPEC-0002 story #9)
// selects its viewer and anchor affordances. Only the values needed by the core
// are enumerated here; the compile-time registry owns the full set.
type ShareType string

const (
	TypeFile       ShareType = "file"       // generic file, non-previewable fallback
	TypeGZ         ShareType = "gz"         // generic gzipped file
	TypeBundle     ShareType = "bundle"     // manifest of ordered members (ADR-0008)
	TypeTrajectory ShareType = "trajectory" // agent run: span tree, no single body (ADR-0009)
	TypeWebhook    ShareType = "webhook"    // live requestbin: capped captured-request stream, no single body (ADR-0010)
)

// Channel is the surface an artifact was created through. It is derived
// server-side from the authenticated surface and is never taken from a client
// claim (ADR-0007 / SPEC-0002 "Channel is server-derived").
type Channel string

const (
	ChannelWeb Channel = "via web"
	ChannelCLI Channel = "via CLI"
	ChannelMCP Channel = "via MCP"
	ChannelAPI Channel = "via API"
)

// Visibility is the link-based access policy. The default is "link":
// you + anyone with the link (ADR-0007).
type Visibility string

const (
	VisibilityLink    Visibility = "link"
	VisibilityPrivate Visibility = "private"
)

// Provenance is the immutable, server-derived record of who created an artifact
// and how. OnBehalfOf names an agent acting for the human ActorID.
type Provenance struct {
	ActorID    string
	OnBehalfOf string
	// Model names the model that produced the artifact, e.g. "claude-opus-5".
	// Empty means the creator did not report one, which is the honest state for
	// a human posting from the CLI — a viewer omits the row rather than showing
	// it blank. It sits on provenance rather than beside it because it answers
	// the same "where did this come from" question as ActorID/OnBehalfOf, is
	// captured once at ingest, and never changes afterwards.
	Model      string
	Channel    Channel
	CapturedAt time.Time
}

// AccessPolicy is the owner + link visibility for an artifact.
type AccessPolicy struct {
	OwnerID    string
	Visibility Visibility
}

// Artifact is the aggregate root. ID is the never-exposed internal primary key;
// PublicID is the opaque base62 handle that appears in every URL. BodySHA256
// references a blob (empty for bundles, whose members are modeled separately).
type Artifact struct {
	ID          int64
	PublicID    string
	ShareType   ShareType
	Title       string
	BodySHA256  string
	Size        int64
	MediaType   string
	Previewable bool
	Provenance  Provenance
	Access      AccessPolicy
	// Tags are client-asserted routing strings, normalized (validated and
	// deduplicated) at create. Deliberately a sibling of Provenance rather than
	// part of it: nothing here is server-derived, so nothing here is
	// trustworthy (ADR-0018).
	Tags []string
	// Denormalized annotation rollups, maintained by the annotation core
	// service in the same transaction as every annotation write so the Bin
	// (`💬 2 · 👀 3`) and artifact headers render with no per-row subquery.
	// They are exposed separately, never summed (ADR-0006 "Count aggregation",
	// SPEC-0006 REQ "Count Aggregation").
	ReactionCount int
	CommentCount  int
	// PinCount counts image-region annotations — image-region reactions plus
	// pinned comments (ADR-0006).
	PinCount int
	// Redaction is what the ingest secret scanner did to this artifact's
	// content: a status, a count and counts per rule ID, never a value
	// (SPEC-0017 RD-9). It is the owner's to see; an adapter shows it only when
	// the viewer owns the artifact. The zero value is stored as "unscanned", so
	// a create path the scanner is not wired into never claims to be clean.
	Redaction redact.Summary
	ExpiresAt time.Time
	CreatedAt time.Time
}

// bodyless reports whether this share type carries no single content-addressed
// body: a bundle (its members are modeled separately), a trajectory (its
// content is the span tree), or a webhook (its content is the captured-request
// stream, filled by third parties after creation rather than pushed by its
// owner — ADR-0010). Every other type MUST reference a body blob.
func (a *Artifact) bodyless() bool {
	return a.ShareType == TypeBundle || a.ShareType == TypeTrajectory || a.ShareType == TypeWebhook
}

// Validate enforces the creation invariants: a public id, a share type, a body
// reference (unless the type is bodyless — a bundle or trajectory), and
// mandatory provenance, access policy, and expiry. It returns a validation-coded
// domain error on the first violation.
func (a *Artifact) Validate() error {
	switch {
	case a.PublicID == "":
		return errs.Validationf("artifact: missing public id")
	case a.ShareType == "":
		return errs.Validationf("artifact: missing share type")
	case !a.bodyless() && a.BodySHA256 == "":
		return errs.Validationf("artifact: missing body reference")
	case a.Provenance.Channel == "":
		return errs.Validationf("artifact: provenance channel is required")
	case a.Provenance.ActorID == "":
		return errs.Validationf("artifact: provenance actor is required")
	case a.Provenance.CapturedAt.IsZero():
		return errs.Validationf("artifact: provenance capture time is required")
	case a.Access.OwnerID == "":
		return errs.Validationf("artifact: access owner is required")
	case a.Access.Visibility == "":
		return errs.Validationf("artifact: access visibility is required")
	case a.ExpiresAt.IsZero():
		return errs.Validationf("artifact: expiry is required")
	}
	return validateNormalizedTags(a.Tags)
}
