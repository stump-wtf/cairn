// Package annotation owns the polymorphic annotation anchor model shared by
// reactions and comments: one embedded anchor of {artifact_id, anchor_type,
// anchor_ref} plus the canonical anchor_key the service derives from it.
//
// This package is the single write-time gate for annotations. Validate checks
// every anchor against the share-type registry's capability matrix and locator
// schemas (nothing is special-cased in code — the webhook "reactable, never
// commentable" asymmetry is registry data), then canonicalizes the locator so
// identity, uniqueness, and grouping never depend on JSON field ordering.
// Domain failures are distinguishable sentinel errors that transport adapters
// map to stable codes without string matching.
//
// Governing: ADR-0006 (Unified Annotation Layer), ADR-0002 (registry-owned
// capabilities), ADR-0012 (error taxonomy), SPEC-0006 REQ "Polymorphic Anchor
// Model", REQ "Registry-Gated Anchor Capabilities", REQ "Webhook Reaction-Only
// Asymmetry", REQ "Error Handling Standards".
package annotation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
	"github.com/stump-wtf/cairn/internal/errs"
	"github.com/stump-wtf/cairn/internal/sharetype"
)

// Sentinel domain errors callers distinguish with errors.Is; all carry the
// validation code, so errs.CodeOf maps any of them to `validation_failed`
// (SPEC-0006 REQ "Error Handling Standards": anchor-not-allowed,
// locator-invalid, comment-on-non-commentable-type are distinguishable).
var (
	// ErrAnchorNotAllowed rejects an anchor_type outside the share type's
	// capability set for the annotation kind.
	ErrAnchorNotAllowed = errs.New(errs.CodeValidation, "anchor type not allowed for share type")
	// ErrLocatorInvalid rejects an anchor_ref that is not valid JSON or fails
	// its anchor_type's locator schema.
	ErrLocatorInvalid = errs.New(errs.CodeValidation, "anchor_ref fails locator schema")
	// ErrNotCommentable rejects a comment on a share type whose registry entry
	// declares no comment anchors at all (the webhook asymmetry).
	ErrNotCommentable = errs.New(errs.CodeValidation, "share type accepts no comment anchors")
)

// Anchor is a validated, canonicalized annotation anchor — the three embedded
// columns both annotation tables share, plus the derived anchor_key. Its full
// identity is the (ArtifactID, Type, Key) triple: ArtifactID scopes it to one
// artifact, Type discriminates the locator schema, and Key is the canonical
// serialization of the locator, so the composite index and the reaction
// uniqueness constraint are deterministic and index-friendly (ADR-0006).
type Anchor struct {
	// ArtifactID is the internal id of the annotated artifact.
	ArtifactID int64
	// Type is the registry-owned anchor_type discriminator.
	Type sharetype.Anchor
	// Ref is the canonicalized anchor_ref locator (sorted keys, no whitespace),
	// suitable for storing in the JSONB column as validated.
	Ref json.RawMessage
	// Key is the canonical anchor_key: string(Ref). Stored alongside Ref so
	// uniqueness and GROUP BY never depend on JSON field ordering.
	Key string
}

// Validate is the registry-gated write-time gate for an annotation anchor: it
// rejects an anchor_type outside the share type's capability set for the kind
// (ErrNotCommentable when the type accepts no comment anchors at all,
// ErrAnchorNotAllowed otherwise), rejects an anchor_ref failing the
// anchor_type's locator schema (ErrLocatorInvalid), and canonicalizes the
// locator into the returned Anchor. Nothing is persisted by callers unless
// Validate accepts (SPEC-0006 "reject the write with validation_failed and
// persist nothing").
//
// Each rejection is a typed violation naming anchor_type or anchor_ref that
// still satisfies errors.Is against its sentinel. A disallowed anchor_type
// names the anchors the share type does take for the kind.
//
// Governing: ADR-0025, SPEC-0019 VE-1, VE-6
func Validate(reg *sharetype.Registry, artifactID int64, shareType artifact.ShareType, kind sharetype.AnnotationKind, anchorType sharetype.Anchor, ref json.RawMessage) (Anchor, error) {
	if !reg.AllowsAnchor(shareType, anchorType, kind) {
		allowed := allowedAnchors(reg, shareType, kind)
		if kind == sharetype.KindComment && len(allowed) == 0 {
			// An omitted anchor_type is not echoed: `"" is not allowed` names no
			// input the caller sent.
			opts := []errs.Opt{errs.WithExpect(fmt.Sprintf("a %s artifact accepts no comments", shareType))}
			if anchorType != "" {
				opts = append(opts, errs.WithValue(string(anchorType)))
			}
			return Anchor{}, fmt.Errorf("annotation: comment on non-commentable share type %q: %w", shareType,
				errs.Violate("anchor_type", errs.LocBody, errs.ReasonNotAllowed, opts...).Because(ErrNotCommentable))
		}
		if anchorType == "" {
			return Anchor{}, fmt.Errorf("annotation: %s with no anchor_type: %w", kind,
				errs.Violate("anchor_type", errs.LocBody, errs.ReasonRequired).Because(ErrAnchorNotAllowed))
		}
		return Anchor{}, fmt.Errorf("annotation: %s on %q anchor for share type %q: %w", kind, anchorType, shareType,
			errs.Violate("anchor_type", errs.LocBody, errs.ReasonNotAllowed, errs.WithValue(string(anchorType)),
				errs.WithExpect(fmt.Sprintf("a %s artifact takes %ss on %s", shareType, kind, strings.Join(allowed, ", ")))).Because(ErrAnchorNotAllowed))
	}
	if err := reg.ValidateLocator(shareType, anchorType, ref); err != nil {
		return Anchor{}, fmt.Errorf("annotation: %q anchor_ref rejected: %v: %w", anchorType, err, locatorViolation(anchorType))
	}
	canonical, err := CanonicalRef(ref)
	if err != nil {
		return Anchor{}, fmt.Errorf("annotation: %q anchor_ref not canonicalizable: %v: %w", anchorType, err, locatorViolation(anchorType))
	}
	return Anchor{
		ArtifactID: artifactID,
		Type:       anchorType,
		Ref:        canonical,
		Key:        string(canonical),
	}, nil
}

// locatorViolation is the anchor_ref violation for a locator that fails its
// anchor_type's schema. The ref is not echoed: it is a JSON object, and a
// 64-byte cut of one explains nothing.
func locatorViolation(anchorType sharetype.Anchor) *errs.Invalid {
	return errs.Violate("anchor_ref", errs.LocBody, errs.ReasonInvalidFormat,
		errs.WithExpect(fmt.Sprintf("the locator object a %q anchor takes", anchorType))).Because(ErrLocatorInvalid)
}

// allowedAnchors lists the anchors on which the share type permits the
// annotation kind, in registry order. Empty means the kind is refused on every
// anchor (the webhook "reactable, never commentable" asymmetry), which picks
// the more specific sentinel; the capability data itself lives entirely in
// the registry.
func allowedAnchors(reg *sharetype.Registry, shareType artifact.ShareType, kind sharetype.AnnotationKind) []string {
	var out []string
	for _, spec := range reg.Resolve(shareType).Anchors() {
		if (kind == sharetype.KindReaction && spec.Reactions) || (kind == sharetype.KindComment && spec.Comments) {
			out = append(out, string(spec.Anchor))
		}
	}
	return out
}

// CanonicalRef returns the canonical serialization of an anchor_ref locator:
// object keys sorted lexicographically at every nesting level, no
// insignificant whitespace, numbers in Go's shortest-round-trip form. An
// absent ref and JSON null canonicalize to the empty object "{}" (the
// whole-artifact locator). Two refs differing only in key order or whitespace
// therefore share one canonical form — the anchor_key the unique index and the
// per-anchor GROUP BY depend on (SPEC-0006 REQ "Polymorphic Anchor Model").
func CanonicalRef(ref json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(ref)) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(ref))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("annotation: anchor_ref is not a JSON object: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("annotation: anchor_ref has trailing data")
	}
	if obj == nil { // JSON null
		return json.RawMessage("{}"), nil
	}
	// encoding/json marshals map keys in sorted order with no whitespace,
	// which is exactly the canonical form.
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("annotation: canonicalize anchor_ref: %w", err)
	}
	return out, nil
}

// CanonicalKey returns the canonical anchor_key for a locator: the string form
// of CanonicalRef.
func CanonicalKey(ref json.RawMessage) (string, error) {
	canonical, err := CanonicalRef(ref)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}
