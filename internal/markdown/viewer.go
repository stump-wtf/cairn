package markdown

import (
	"html/template"
	"strings"
)

// fragmentTmpl is the server-rendered body fragment the app shell drops into its
// body slot for a markdown artifact (ADR-0011: server-rendered html/template, no
// build step). It carries three regions:
//
//   - a collapsible multi-level CONTENTS table of contents whose entries are
//     in-page links to their heading blocks (SPEC-0003 REQ "Markdown Viewer");
//     built from native <details>/<summary> so the caret exposes aria-expanded
//     and the whole TOC is keyboard-operable with no JavaScript;
//   - the prose, one wrapper per top-level block carrying the deterministic
//     block_id and a react `＋` affordance that emits an md_block anchor for
//     SPEC-0006 to persist (REQ "Markdown Annotation Anchors").
//
// The block HTML is already goldmark-rendered and bluemonday-sanitized, so it is
// emitted verbatim; everything else is auto-escaped by html/template. The
// affordances and typography are progressive: the prose reads with no
// JavaScript, and md.js only layers on the reaction picker, the md_bullet
// affordance, and the selection composer (SPEC-0003 REQ "Progressive
// Enhancement").
var fragmentTmpl = template.Must(template.New("md").Parse(`
{{define "toc-entries"}}<ol class="md-toc-list">{{range .}}<li class="md-toc-item">
<a class="md-toc-link" href="#{{.BlockID}}" data-level="{{.Level}}">{{.Text}}</a>
{{if .Children}}{{template "toc-entries" .Children}}{{end}}
</li>{{end}}</ol>{{end}}

{{define "md"}}<div class="md-viewer" data-md-viewer>
{{if .TOC}}<nav class="md-toc" aria-label="Table of contents">
<details class="md-toc-fold" open>
<summary class="md-toc-summary">Contents</summary>
{{template "toc-entries" .TOC}}
</details>
<p class="md-stats" aria-label="Document statistics">{{.Stats.Words}} words · {{.Stats.Headings}} headings · {{.Stats.ReadMins}} min read</p>
</nav>{{end}}
<div class="md-prose" data-md-prose>
{{range .Blocks}}<div class="md-block{{if .IsList}} md-block-list{{end}}" id="{{.BlockID}}" data-block-id="{{.BlockID}}"{{if .IsList}} data-md-list="true"{{end}}>
<div class="md-block-body">{{.HTML}}</div>
<button type="button" class="md-react" data-anchor-type="md_block" data-block-id="{{.BlockID}}" aria-label="React to this block">＋</button>
</div>
{{end}}</div>
</div>{{end}}
`))

// Fragment renders a previously-parsed document into the sanitized body-slot
// HTML fragment. The result is trusted template output: the per-block HTML was
// sanitized in Render and the chrome is auto-escaped here, so the shell emits it
// verbatim into its body slot.
func Fragment(r *Rendered) (template.HTML, error) {
	var buf strings.Builder
	if err := fragmentTmpl.ExecuteTemplate(&buf, "md", r); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // sanitized content + escaped chrome
}

// RenderFragment is the one-shot convenience the BodyViewer uses: parse a
// markdown body and render its body-slot fragment in a single call.
func RenderFragment(source []byte) (template.HTML, error) {
	r, err := Render(source)
	if err != nil {
		return "", err
	}
	return Fragment(r)
}
