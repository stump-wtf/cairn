// Package imageview renders the image share type's server-side body-slot
// fragment (SPEC-0003 REQ "Image Viewer", issue #69): the image itself plus
// the static scaffolding image.js progressively enhances into the bespoke
// pin overlay and the whole-artifact react-below affordance. It mirrors
// internal/markdown's split — rendering isolated from the registry glue in
// internal/sharetype/builtin.go — even though an image body needs no parsing:
// the image bytes stream from the same content-addressed store every other
// viewer's body does, through the dedicated inline-serving route (httpapi's
// InlineViewer capability) instead of the sniff-proof generic download every
// other type gets, so `<img src>` paints on-page instead of forcing an
// attachment download.
//
// Existing pins (image_region-anchored comments) and the react-below tallies
// are deliberately NOT rendered here: RenderBody's signature is body bytes
// in, HTML out — it carries no annotation-service seam (ADR-0002's BodyViewer
// is intentionally that narrow, the same reason the bundle viewer's member
// rail is built in httpapi rather than through this capability). image.js
// hydrates both client-side instead: pins by reading the `data-pin-x`/
// `data-pin-y` attributes the shell's comment-item partial already carries on
// every image_region comment (SPEC-0006 — the panel thread is already
// server-rendered, so this costs no extra round-trip), and reaction pills by
// fetching `GET /v1/artifacts/{id}/reactions` (the same JSON surface
// trajectory.js/markdown.js already read reaction/comment data from).
//
// If image.js fails to load entirely, the `<img>` still renders (a plain
// same-origin element needing no script — SPEC-0003 Scenario "Pin overlay
// unavailable") and the panel's HTMX comment composer still posts a
// whole-artifact comment with no image.js involved at all; only the
// interactive pin-drop and the reaction "+" affordance go inert, and the
// reaction total is still visible with no JS via the MetadataPanel
// "reactions" field (internal/sharetype/builtin.go).
//
// Governing: ADR-0011 (server-rendered viewer, no build step, bespoke
// vanilla-JS/SVG widgets), ADR-0006 (normalized fractional pin coordinates),
// SPEC-0003 REQ "Image Viewer", REQ "Progressive Enhancement".
package imageview

import (
	"html/template"
	"strings"

	"github.com/stump-wtf/cairn/internal/artifact"
)

// fragmentTmpl is the whole fragment: the image inside its pin-drop frame (an
// empty overlay layer image.js populates), and the react-below toolbar (an
// empty pill cluster image.js hydrates from the reaction tallies). Every
// value it interpolates is server-computed from the artifact (id, title), so
// html/template's auto-escaping is the only escaping this needs — no user
// body content is ever emitted here (the image bytes stream separately,
// through the inline body route the `src` merely links to).
var fragmentTmpl = template.Must(template.New("image").Parse(`
{{define "image"}}<div class="img-viewer" data-img-viewer data-artifact-id="{{.ID}}">
<div class="img-frame" data-img-frame tabindex="0" role="group" aria-label="{{.Alt}}. Click the image, or press Enter, to drop a pin and comment on that spot.">
<img class="img-body" data-img-el src="{{.ImageURL}}" alt="{{.Alt}}">
<div class="img-pins" data-img-pins aria-hidden="true"></div>
</div>
<div class="img-toolbar">
<p class="img-hint">Click the image to drop a pin and comment on that spot.</p>
<div class="img-react-cluster" data-img-react-cluster role="group" aria-label="React to this image">
<button type="button" class="img-react-add" data-img-react-add aria-label="Add a reaction" aria-haspopup="true">＋ react</button>
</div>
</div>
</div>{{end}}
`))

// view is fragmentTmpl's input: the id (for the pin/react wiring's
// data-artifact-id and the inline body URL), an accessible alt text, and the
// same-origin inline image URL (SPEC-0003, ADR-0007 canonical bare-scheme id
// space — this is never absolute, so it works identically behind any origin
// the shell itself is served from).
type view struct {
	ID       string
	Alt      string
	ImageURL string
}

// Fragment renders the image body-slot fragment for an already-classified,
// previewable image artifact (DecidePreview only ever resolves an artifact to
// the image share type when its media type passed isImage at ingest —
// SPEC-0002 "Previewability Detection at Ingest" — so by the time this runs
// the artifact is known-good).
func Fragment(a *artifact.Artifact) (template.HTML, error) {
	var buf strings.Builder
	v := view{
		ID:       a.PublicID,
		Alt:      altText(a),
		ImageURL: "/" + a.PublicID + "/image",
	}
	if err := fragmentTmpl.ExecuteTemplate(&buf, "image", v); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // chrome is auto-escaped; no raw user content is emitted
}

// altText falls back to the public id when the artifact carries no title, so
// the <img> always has a non-empty accessible name (WCAG 1.1.1).
func altText(a *artifact.Artifact) string {
	if a.Title != "" {
		return a.Title
	}
	return a.PublicID
}
