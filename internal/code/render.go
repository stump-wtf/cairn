// Package code renders a code artifact body into the server-side, line
// numbered, syntax-highlighted HTML fragment the app shell drops into its
// body slot, together with a navigable symbol outline and the facts the
// metadata panel shows — the code-viewer analogue of internal/markdown
// (SPEC-0003 REQ "Code Viewer").
//
// Highlighting is done entirely on the server with a pure-Go lexer
// (github.com/alecthomas/chroma/v2), so a viewer with JavaScript disabled
// still gets a highlighted, line-numbered, readable rendering — only the
// interactive line-comment/react affordances are JS-layered (SPEC-0003 REQ
// "Progressive Enhancement"). chroma's HTML formatter HTML-escapes every
// token's text, so the rendered fragment can never execute source content as
// markup (SPEC-0003 Security: "source is escaped, never executed") — the same
// "trusted because already-escaped" contract internal/markdown's sanitizer
// establishes for its own package.
//
// Governing: ADR-0011 (server-rendered viewer fragments, no build step),
// ADR-0006 (deterministic, stable anchors), SPEC-0003 REQ "Code Viewer", REQ
// "Code Annotation Anchors", REQ "Progressive Enhancement", Security
// Requirements.
package code

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// chromaStyle is the single dark syntax theme every code artifact renders
// with. The app shell is dark-only (base.html sets
// `<meta name="color-scheme" content="dark">`), so one style — rather than a
// light/dark pair — is sufficient; github-dark reads well against the shell's
// near-black well/panel tokens (app.css --well #0F1319).
var chromaStyle = styles.Get("github-dark")

// chromaClassPrefix namespaces every emitted class (e.g. "chroma-kd") so
// chroma's short, generic token classes ("kd", "nx", "s") can never collide
// with the shell's own CSS.
const chromaClassPrefix = "chroma-"

// formatter emits classed inline spans with no surrounding <pre>/<code> and no
// inline `style="…"` attributes (WithClasses, PreventSurroundingPre): every
// color comes from the pre-generated, embedded web/assets/chroma.css
// (cmd/gen-chroma-css), which is the only way to highlight under a
// `style-src 'self'` CSP with no external stylesheet and no per-element
// inline style (SPEC-0003 "NO external CDN — CSP is script-src/style-src
// 'self'").
var formatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.ClassPrefix(chromaClassPrefix),
	chromahtml.PreventSurroundingPre(true),
)

// maxOutlineSymbols caps the symbol outline so a pathological file (a
// minified bundle full of one-line "function" matches) cannot blow up the
// rendered panel.
const maxOutlineSymbols = 300

// Line is one highlighted, line-numbered row of source: its 1-based line
// number and its chroma-highlighted, already-escaped HTML.
type Line struct {
	Num  int
	HTML template.HTML
}

// Rendered is the full result of rendering a code body: the highlighted
// lines, the resolved language, the derived symbol outline, and the raw
// source text (so the client's select-to-comment affordance can compute
// text_selection offsets against the canonical body — unlike markdown's
// goldmark transform, highlighting never rewrites the source text, so these
// offsets land on the exact same characters the stored body carries).
type Rendered struct {
	Lines     []Line
	Language  Language
	Outline   []Symbol
	Source    string
	LineCount int
}

// Render highlights a code body into its line-numbered rows, resolves its
// language (Detect's override/title/media-type precedence), and derives its
// symbol outline. Rendering never fails on adversarial or binary-ish input —
// an unrecoverable lex degrades to the plaintext lexer rather than an error —
// so a code artifact of any content always renders (mirrors
// internal/markdown.Render's "never fails" contract).
func Render(source []byte, mediaType, title, langOverride string) (*Rendered, error) {
	lang := Detect(langOverride, mediaType, title)
	src := string(source)

	tokens, err := chroma.Tokenise(chroma.Coalesce(lang.Lexer), nil, src)
	if err != nil {
		tokens, err = chroma.Tokenise(lexers.Fallback, nil, src)
		if err != nil {
			// lexers.Fallback is chroma's own plaintext rule set; failing to
			// tokenise plain text would be a chroma bug, not adversarial
			// input. Degrade to one unhighlighted line rather than erroring
			// the whole viewer.
			tokens = []chroma.Token{{Type: chroma.Text, Value: src}}
		}
	}

	out := &Rendered{Language: lang, Source: src}
	if len(src) == 0 {
		return out, nil
	}

	lineToks := chroma.SplitTokensIntoLines(tokens)
	out.Lines = make([]Line, 0, len(lineToks))
	for i, lt := range lineToks {
		lt = trimLineEnding(lt)
		var buf bytes.Buffer
		if err := formatter.Format(&buf, chromaStyle, chroma.Literator(lt...)); err != nil {
			// A formatter failure on one line degrades that line to plain
			// (escaped) text rather than dropping the whole render.
			buf.Reset()
			buf.WriteString(template.HTMLEscapeString(chroma.Stringify(lt...)))
		}
		out.Lines = append(out.Lines, Line{Num: i + 1, HTML: template.HTML(buf.String())}) //nolint:gosec // chroma HTML-escapes every token value
	}
	out.LineCount = len(out.Lines)

	out.Outline = Outline(src, lang.Key)
	if len(out.Outline) > maxOutlineSymbols {
		out.Outline = out.Outline[:maxOutlineSymbols]
	}
	return out, nil
}

// trimLineEnding strips the trailing "\n"/"\r\n" SplitTokensIntoLines leaves
// on a line's final token, so a rendered line's HTML carries no stray
// newline character (which would otherwise land in the DOM text stream the
// select-to-comment affordance walks, shifting its offsets by one per line).
func trimLineEnding(line []chroma.Token) []chroma.Token {
	if len(line) == 0 {
		return line
	}
	last := line[len(line)-1]
	trimmed := strings.TrimRight(last.Value, "\r\n")
	if trimmed == last.Value {
		return line
	}
	if trimmed == "" {
		return line[:len(line)-1]
	}
	out := make([]chroma.Token, len(line))
	copy(out, line)
	out[len(out)-1] = chroma.Token{Type: last.Type, Value: trimmed}
	return out
}
