package code

import (
	"html/template"
	"strconv"
	"strings"
)

// fragmentTmpl is the server-rendered body fragment the app shell drops into
// its body slot for a code artifact (ADR-0011: server-rendered html/template,
// no build step), mirroring internal/markdown's fragmentTmpl. It carries:
//
//   - a collapsible SYMBOLS outline (native <details>/<summary>, so it is
//     keyboard-operable with no JavaScript) whose entries are in-page links to
//     their line anchors (SPEC-0003 REQ "Code Viewer": "a navigable list of
//     top-level symbols that jump to their line");
//   - a STATS line (language · line count), the code analogue of markdown's
//     "words · headings · min read" line — this is where "line count" is
//     shown rather than the registry MetadataPanel capability, because
//     MetadataPanel(a *artifact.Artifact) has no body access and line count
//     cannot be derived without reading the body (see codeShareType.
//     MetadataPanel in internal/sharetype/builtin.go for the fields that CAN
//     be derived from the artifact alone: language);
//   - the highlighted source as one <table> row per line, each row carrying
//     its line number as an in-page anchor id/href and react `＋`/comment `💬`
//     affordances that emit code_line anchors (SPEC-0003 REQ "Code
//     Annotation Anchors"), plus an (initially empty) annotations slot code.js
//     fills with persisted reaction pills.
//
// The per-line HTML is already chroma-rendered (which HTML-escapes every
// token, see render.go), so it is emitted verbatim; everything else is
// auto-escaped by html/template. Line-range reactions (code_range) and
// select-to-comment (text_selection) are progressive JS layered on top
// (code.js) — the highlighted, line-numbered source itself reads with no
// JavaScript (SPEC-0003 REQ "Progressive Enhancement").
var fragmentTmpl = template.Must(template.New("code").Funcs(template.FuncMap{
	"lineID": lineID,
}).Parse(`
{{define "code"}}<div class="code-viewer" data-code-viewer data-lang="{{.Language.Key}}">
{{if .Outline}}<nav class="code-outline" aria-label="Symbol outline">
<details class="code-outline-fold" open>
<summary class="code-outline-summary">Symbols</summary>
<ol class="code-outline-list">
{{range .Outline}}<li class="code-outline-item"><a class="code-outline-link" href="#{{lineID .Line}}" data-line="{{.Line}}"><span class="code-outline-kind">{{.Kind}}</span> <span class="code-outline-name">{{.Name}}</span></a></li>
{{end}}</ol>
</details>
</nav>{{end}}
<p class="code-stats" aria-label="Code statistics">{{.Language.Display}} · {{.LineCount}} line{{if ne .LineCount 1}}s{{end}}</p>
<div class="code-source-wrap" data-code-source>
<table class="code-table chroma-chroma" data-code-table>
<tbody>
{{range .Lines}}<tr class="code-row" id="{{lineID .Num}}" data-line="{{.Num}}">
<td class="code-lineno" data-code-lineno><a class="code-lineno-link" href="#{{lineID .Num}}" tabindex="-1" aria-label="Line {{.Num}}">{{.Num}}</a></td>
<td class="code-line-cell">
<span class="code-line-code">{{.HTML}}</span><button type="button" class="code-react" data-anchor-type="code_line" data-line="{{.Num}}" aria-label="React to line {{.Num}}">＋</button><button type="button" class="code-react code-comment-btn" data-line="{{.Num}}" aria-label="Comment on line {{.Num}}">💬</button>
<div class="code-annotations" data-code-annotations data-line="{{.Num}}"></div>
</td>
</tr>
{{end}}</tbody>
</table>
</div>
</div>{{end}}
`))

// lineID renders a code line's in-page anchor id, "L42" — GitHub's own
// convention, which readers pasting a code_line comment link will recognize.
func lineID(n int) string { return "L" + strconv.Itoa(n) }

// Fragment renders a previously-parsed code body into the body-slot HTML
// fragment. The result is trusted template output: the per-line HTML was
// escaped by chroma in Render, and the chrome is auto-escaped here, so the
// shell emits it verbatim into its body slot.
func Fragment(r *Rendered) (template.HTML, error) {
	var buf strings.Builder
	if err := fragmentTmpl.ExecuteTemplate(&buf, "code", r); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // sanitized content + escaped chrome
}

// RenderFragment is the one-shot convenience the BodyViewer capability uses:
// highlight a code body and render its body-slot fragment in a single call.
func RenderFragment(source []byte, mediaType, title, langOverride string) (template.HTML, error) {
	r, err := Render(source, mediaType, title, langOverride)
	if err != nil {
		return "", err
	}
	return Fragment(r)
}
