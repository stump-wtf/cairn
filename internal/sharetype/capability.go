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

// ComposedViewer is the optional capability a share type implements when its
// body is not a single content-addressed blob but an ordered set of member files
// — a bundle (ADR-0008). The shell resolves it through the registry (never a
// switch on the type key) to render the member file rail and delegate each
// selected member back through the registry to that member's own viewer
// (ADR-0002 "bundle is the one type aware of others' viewers"; SPEC-0003 REQ
// "Bundle Viewer": "delegating back through the registry"). The capability
// itself carries only the anchor_type a whole-member annotation attaches to
// (bundle_file); enumerating members and reading member bodies is the store's job
// in httpapi, exactly as the trajectory viewer resolves its run tree through the
// trajectory service rather than through a single-body seam.
type ComposedViewer interface {
	// MemberAnchor is the anchor_type a whole-member reaction/comment attaches to.
	MemberAnchor() Anchor
}

// ComposedViewerFor resolves key and returns its ComposedViewer capability, or
// (nil, false) when the type renders from a single body (or none). Total, like
// Resolve, so httpapi picks the bundle path off registry data — not a type key.
func (r *Registry) ComposedViewerFor(key artifact.ShareType) (ComposedViewer, bool) {
	v, ok := r.Resolve(key).(ComposedViewer)
	return v, ok
}

// StreamViewer is the optional capability a share type implements when its
// body is neither a single content-addressed blob nor a bundle's ordered
// member set, but a live, continuously-updating log reconstructed from its
// own service state — a webhook's captured-request ring buffer (ADR-0010,
// SPEC-0005). Unlike ComposedViewer's static member set, a stream viewer's
// content keeps growing after the artifact is created, fanned out over SSE
// (ADR-0012); the capability itself carries only the anchor a single
// streamed item's whole-item reaction attaches to (webhook_request),
// mirroring ComposedViewer.MemberAnchor's shape. Enumerating and rendering
// stream items is the type's own service's job in httpapi (s.hook), exactly
// as the bundle/trajectory viewers resolve their own state through their own
// services rather than through a single-body seam (issue #86).
type StreamViewer interface {
	// ItemAnchor is the anchor_type a whole-item reaction attaches to.
	ItemAnchor() Anchor
}

// StreamViewerFor resolves key and returns its StreamViewer capability, or
// (nil, false) when the type does not render a live stream. Total, like
// Resolve, so httpapi picks the webhook viewer path off registry data — never
// a switch on the type key.
func (r *Registry) StreamViewerFor(key artifact.ShareType) (StreamViewer, bool) {
	v, ok := r.Resolve(key).(StreamViewer)
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

// InlineViewer is the optional capability through which a share type declares
// that its content-addressed body must stream inline — the real Content-Type,
// `Content-Disposition: inline` — rather than through the sniff-proof generic
// download every other bodied type gets (fileCard's `/{id}/download`, the
// ADR-0002 total-resolution floor). Only the image type implements it
// (SPEC-0003 REQ "Image Viewer"): its BodyViewer fragment embeds an `<img>`
// pointing back at its own body, and a browser never executes active content
// loaded through an `<img>` element — even `image/svg+xml` is rendered, not
// scripted, in that context — so serving the real media type inline cannot
// execute untrusted content in Cairn's origin.
type InlineViewer interface {
	// InlineBody reports whether a's body should stream inline. A type MAY key
	// this off the concrete artifact (re-checking the media type) rather than
	// being unconditionally true.
	InlineBody(a *artifact.Artifact) bool
}

// InlineBodyFor resolves the artifact's type and reports whether its body
// should stream inline, or false when the type does not implement
// InlineViewer — the default, so every non-image type keeps the sniff-proof
// download. Total, like BadgeFor: httpapi calls this instead of switching on
// the type key (ADR-0002).
func (r *Registry) InlineBodyFor(a *artifact.Artifact) bool {
	if v, ok := r.Resolve(a.ShareType).(InlineViewer); ok {
		return v.InlineBody(a)
	}
	return false
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
