// Package sharetype is the compile-time share-type registry: the one place a
// stored share type is mapped to its viewer/panel/anchor affordances. Share
// types are Go values implementing ShareType and registered at init; the core
// and shell resolve a handler through the registry and never switch on the type.
// Resolution is total — an unregistered or uninterpretable type resolves to the
// built-in generic file handler, so every artifact stays viewable, downloadable,
// and annotatable at the whole-artifact level.
//
// The registry is also the single source of truth for two cross-cutting rules
// SPEC-0002 assigns to the type, not the viewer:
//   - which annotation anchors are legal for a type (SPEC-0006 validates against
//     this, so a viewer and its validator cannot drift); and
//   - whether a body is previewable, decided at ingest from the share type plus
//     the sniffed/declared media type and a size bound, and stored on the
//     artifact so every surface agrees without re-sniffing.
//
// Governing: ADR-0002 (Extensible Share-Type Model & Viewer Registry),
// SPEC-0002 REQ "Share-Type Registry and Total Resolution",
// REQ "Per-Type Anchor Affordances", REQ "Previewability Detection at Ingest".
package sharetype

import (
	"fmt"
	"strings"
	"sync"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// ShareType is the single base interface every kind satisfies. A type sees only
// its own identity, badge, previewability predicate, and legal anchors; it
// never touches the Artifact aggregate or the storage layer, so adding a type
// is purely additive (ADR-0002). The base contract is deliberately narrow and
// frozen: everything else a type may contribute — URL prefixes, a body viewer,
// metadata-panel fields, locator schemas, service routes, artifact-dependent
// badges — is an OPTIONAL capability interface discovered by type assertion at
// the registry seam (see capability.go), never a new method here.
type ShareType interface {
	// Key is the registry key stored in artifacts.share_type.
	Key() artifact.ShareType
	// Badge is the short human/agent-facing label (e.g. "MD", "IMG", "HK",
	// "TRJ"). A type whose badge depends on the concrete artifact (code's lang
	// badge) additionally implements ArtifactBadger; this is the static default.
	Badge() string
	// PreviewableMedia reports whether a rich viewer exists for this type paired
	// with the given (sniffed/declared) media type. The registry additionally
	// applies the size bound before recording previewable=true.
	PreviewableMedia(mediaType string) bool
	// Anchors declares the annotation anchors legal for this type, including the
	// whole-artifact `artifact` anchor and which kinds each permits. The full
	// capability matrix is registry data (SPEC-0006 REQ "Registry-Gated Anchor
	// Capabilities"); nothing — not even whole-artifact — is implied centrally,
	// which is how webhook rejects all comments while staying reactable.
	Anchors() []AnchorSpec
}

// Registry is a compile-time registry of share types. It is safe for concurrent
// resolution; registration happens at init (single-threaded) but is guarded too.
type Registry struct {
	mu       sync.RWMutex
	types    map[artifact.ShareType]ShareType
	fallback ShareType
}

// NewRegistry returns an empty registry whose total-resolution fallback is the
// given generic file handler.
func NewRegistry(fallback ShareType) *Registry {
	return &Registry{
		types:    make(map[artifact.ShareType]ShareType),
		fallback: fallback,
	}
}

// Register adds a share type. Registering the same key twice is a programming
// error (the registry is closed for modification) and panics, matching the
// compile-time, single-binary integration model of ADR-0002.
func (r *Registry) Register(t ShareType) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := t.Key()
	if key == "" {
		panic("sharetype: cannot register a type with an empty key")
	}
	if _, dup := r.types[key]; dup {
		panic(fmt.Sprintf("sharetype: duplicate registration for %q", key))
	}
	r.types[key] = t
}

// Resolve maps a stored share type to its handler. Resolution is total: an
// unregistered or empty key resolves to the generic file fallback so the
// artifact stays viewable and downloadable (SPEC-0002 "Unknown type resolves to
// generic file"). Callers MUST resolve through this method rather than switching
// on the type set (SPEC-0002 "No switch on type").
func (r *Registry) Resolve(key artifact.ShareType) ShareType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.types[key]; ok {
		return t
	}
	return r.fallback
}

// Registered reports whether key names a registered type (as opposed to
// resolving only via the fallback). Used by previewability detection to decide
// whether to keep the declared type or downgrade to the generic file type.
func (r *Registry) Registered(key artifact.ShareType) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.types[key]
	return ok
}

// DecidePreview decides, at ingest, the effective share type and previewability
// for a single body. A body is previewable only when its declared type is
// registered, a viewer exists for the type/media pair, and the body is within
// the preview size bound; otherwise the artifact becomes the generic file type
// (GZ for gzip, else FILE) with previewable=false. The decision is stored so
// every surface agrees without re-sniffing (SPEC-0002 "Previewability Detection
// at Ingest").
func (r *Registry) DecidePreview(declared artifact.ShareType, mediaType string, size, previewMax int64) (artifact.ShareType, bool) {
	handler := r.Resolve(declared)
	previewable := r.Registered(declared) &&
		handler.PreviewableMedia(mediaType) &&
		(previewMax <= 0 || size <= previewMax)
	if previewable {
		return declared, true
	}
	return genericFileType(mediaType), false
}

// AllowsAnchor reports whether an annotation of the given kind may target the
// given anchor on an artifact of the given type. The entire capability matrix —
// including whole-artifact legality — is per-type registry data with no central
// carve-outs, so the webhook type can reject every comment anchor (SPEC-0006
// REQ "Webhook Reaction-Only Asymmetry") while the generic-file fallback keeps
// unknown types annotatable at the whole-artifact level. This is the single
// source of truth the annotation layer (SPEC-0006) validates against.
func (r *Registry) AllowsAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) bool {
	handler := r.Resolve(key)
	for _, spec := range handler.Anchors() {
		if spec.Anchor != anchor {
			continue
		}
		switch kind {
		case KindReaction:
			return spec.Reactions
		case KindComment:
			return spec.Comments
		default:
			return false
		}
	}
	return false
}

// ValidateAnchor returns a validation-coded domain error when an annotation of
// kind may not target anchor on an artifact of the given type, or nil when it is
// legal. SPEC-0006 calls this so a viewer and its validator cannot drift.
func (r *Registry) ValidateAnchor(key artifact.ShareType, anchor Anchor, kind AnnotationKind) error {
	if r.AllowsAnchor(key, anchor, kind) {
		return nil
	}
	return errs.Validationf("sharetype: %s not permitted against %q anchor for type %q", kind, anchor, key)
}

// ClassifyMember resolves a bundle member's own share type from its in-bundle
// file name and (sniffed) media type, so the bundle viewer can delegate the
// member back through the registry to that type's viewer (SPEC-0003 "Bundle
// delegates back through the registry"). Ingest sniffs member bodies with
// http.DetectContentType, which reports markdown/plain text as `text/plain`, so
// markdown is recognized by its `.md`/`.markdown` extension (or an explicit
// markdown media type) rather than by sniffing. In 0.0.2 markdown is the only
// rich member viewer, so every other member resolves to the generic file type —
// the total-resolution floor (ADR-0002); later member viewers (code, image)
// extend this mapping, and the shell keeps delegating through the registry.
func (r *Registry) ClassifyMember(name, mediaType string) artifact.ShareType {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") ||
		strings.Contains(strings.ToLower(mediaType), "markdown") {
		if r.Registered(KeyMarkdown) {
			return KeyMarkdown
		}
	}
	return genericFileType(mediaType)
}

// genericFileType is the generic file share type for a non-previewable body:
// GZ for gzip bodies, FILE otherwise (SPEC-0002 "FILE/GZ").
func genericFileType(mediaType string) artifact.ShareType {
	m := strings.ToLower(mediaType)
	if strings.Contains(m, "gzip") || strings.Contains(m, "x-gzip") {
		return artifact.TypeGZ
	}
	return artifact.TypeFile
}
