package sharetype

// Optional capability interfaces a share type MAY additionally implement.
//
// ADR-0002 fixes the base ShareType contract conservatively (identity, badge,
// previewability, anchors) and grows it "via optional capability interfaces
// rather than by widening the base contract": a capability is discovered by
// type assertion at the registry seam, so an existing type that does not need
// one compiles unchanged, and a new type composes exactly the capabilities it
// wants. The registry exposes one total helper per capability (URLPrefixFor,
// BadgeFor, BodyViewerFor, MetadataPanelFor, ValidateLocator, MountRoutes) so
// core packages resolve behavior through the registry and never type-assert —
// or switch on a share-type key — themselves.
//
// Governing: ADR-0002 (grow via optional capability interfaces; no switch on
// type outside a type's own package), ADR-0011 (server-rendered viewer/panel
// fragments), ADR-0012 (chi sub-routers), SPEC-0002 REQ "Share-Type Registry
// and Total Resolution", SPEC-0006 REQ "Registry-Gated Anchor Capabilities".

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/cairn/internal/artifact"
	"github.com/joestump/cairn/internal/errs"
)

// URLPrefix is the pair of legible sub-path segments a share type contributes
// to the ADR-0005 URL scheme. The zero value means the bare scheme:
// cairn.sh/<id> and mcp://cairn/<id>. Trajectory sets {Web: "run", MCP: "run"};
// webhook sets {MCP: "hook"} (its web URL stays bare). Segments carry no
// slashes; the consumer joins them.
type URLPrefix struct {
	Web string
	MCP string
}

// URLPrefixer is the optional capability through which a share type declares
// its URL sub-prefixes as registry data. Types that live at the bare id simply
// do not implement it.
type URLPrefixer interface {
	URLPrefix() URLPrefix
}

// URLPrefixFor resolves key and returns its URL prefixes, or the zero (bare)
// prefix when the type does not implement URLPrefixer. Total, like Resolve.
func (r *Registry) URLPrefixFor(key artifact.ShareType) URLPrefix {
	if p, ok := r.Resolve(key).(URLPrefixer); ok {
		return p.URLPrefix()
	}
	return URLPrefix{}
}

// ArtifactBadger is the optional capability for badges that depend on the
// concrete artifact rather than the type alone — the code type's per-language
// badge (`GO`, `PY`, …) per ADR-0002's "`PY`/lang". Returning "" defers to the
// type's static Badge().
type ArtifactBadger interface {
	BadgeFor(a *artifact.Artifact) string
}

// BadgeFor returns the badge for a concrete artifact: the type's dynamic
// ArtifactBadger badge when implemented and non-empty, else its static Badge().
func (r *Registry) BadgeFor(a *artifact.Artifact) string {
	t := r.Resolve(a.ShareType)
	if b, ok := t.(ArtifactBadger); ok {
		if badge := b.BadgeFor(a); badge != "" {
			return badge
		}
	}
	return t.Badge()
}

// BodyViewer is the optional capability through which a share type renders its
// server-side body fragment for the app shell's body slot (ADR-0011). The body
// reader streams from the content-addressed store (ADR-0008); the fragment is
// trusted template output and MUST already be escaped/sanitized by the viewer.
// Concrete viewers land with the viewer stories (SPEC-0003/0004/0005); a type
// without a viewer falls back to the shell's generic file card.
type BodyViewer interface {
	RenderBody(ctx context.Context, a *artifact.Artifact, body io.Reader) (template.HTML, error)
}

// BodyViewerFor resolves key and returns its BodyViewer capability, or
// (nil, false) when the type renders via the generic file card.
func (r *Registry) BodyViewerFor(key artifact.ShareType) (BodyViewer, bool) {
	v, ok := r.Resolve(key).(BodyViewer)
	return v, ok
}

// PanelField is one label/value row a type contributes to the metadata panel's
// type-specific section (a file's checksum, a trajectory's run stats, an
// image's pin count). Core panel sections — provenance, access, expiry,
// comments — are supplied by the shell, never by a type (ADR-0002).
type PanelField struct {
	Label string
	Value string
}

// MetadataPaneler is the optional capability through which a share type
// contributes its type-specific metadata-panel fields.
type MetadataPaneler interface {
	MetadataPanel(a *artifact.Artifact) []PanelField
}

// MetadataPanelFor resolves the artifact's type and returns its type-specific
// panel fields, or nil when the type contributes none.
func (r *Registry) MetadataPanelFor(a *artifact.Artifact) []PanelField {
	if p, ok := r.Resolve(a.ShareType).(MetadataPaneler); ok {
		return p.MetadataPanel(a)
	}
	return nil
}

// LocatorFunc validates one anchor_ref locator payload, returning nil when the
// payload matches the anchor's schema.
type LocatorFunc func(ref json.RawMessage) error

// LocatorSchemer is the optional capability through which a share type supplies
// per-anchor locator schemas for its anchor_ref payloads (SPEC-0006 REQ
// "Registry-Gated Anchor Capabilities": "reject an anchor_ref that fails that
// anchor_type's locator schema"). Returning nil for an anchor means the type
// declares no schema for it and any payload is accepted (the annotations story
// tightens each anchor as its viewer lands).
type LocatorSchemer interface {
	LocatorSchema(anchor Anchor) LocatorFunc
}

// ValidateLocator validates an anchor_ref against the anchor's locator schema
// for the given type. The whole-artifact `artifact` anchor is validated by the
// registry itself — its anchor_ref MUST be the empty object `{}` (SPEC-0006
// "Whole-artifact anchor") — since that rule is type-agnostic. All other
// anchors defer to the type's LocatorSchemer capability when present.
func (r *Registry) ValidateLocator(key artifact.ShareType, anchor Anchor, ref json.RawMessage) error {
	if anchor == AnchorArtifact {
		return validateEmptyObjectRef(ref)
	}
	if ls, ok := r.Resolve(key).(LocatorSchemer); ok {
		if fn := ls.LocatorSchema(anchor); fn != nil {
			return fn(ref)
		}
	}
	return nil
}

// validateEmptyObjectRef accepts only an absent ref or the empty JSON object.
func validateEmptyObjectRef(ref json.RawMessage) error {
	if len(ref) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(ref, &obj); err != nil || len(obj) != 0 {
		return errs.Validationf("sharetype: anchor_ref for %q anchor must be the empty object {}", AnchorArtifact)
	}
	return nil
}

// RouteMounter is the optional capability through which a share type
// contributes its own service surface — e.g. trajectory's /v1/runs* ingest
// routes — to the API router. The type captures its own dependencies at
// registration time; httpapi core is never edited to add a type's routes.
type RouteMounter interface {
	MountRoutes(r chi.Router)
}

// MountRoutes gives every registered type implementing RouteMounter a chance to
// contribute routes, in deterministic (key-sorted) order. httpapi calls this
// once while building its router; a new share type's ingest surface therefore
// arrives by registration alone (ADR-0002 "registration is the only
// integration point").
func (r *Registry) MountRoutes(router chi.Router) {
	for _, t := range r.Types() {
		if m, ok := t.(RouteMounter); ok {
			m.MountRoutes(router)
		}
	}
}

// Types returns every registered share type sorted by key, so capability
// iteration (route mounting, conformance tests) is deterministic.
func (r *Registry) Types() []ShareType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ShareType, 0, len(r.types))
	for _, t := range r.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}
