package markdown

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// gm is the shared goldmark converter. It enables GitHub-flavored markdown
// (tables, strikethrough, task lists, autolinks) but deliberately does NOT set
// html.WithUnsafe(): raw HTML blocks and inline HTML in the body are omitted
// rather than passed through, which is the first of two sanitization layers
// (bluemonday is the second). WithHardWraps is off so prose wraps the way
// markdown intends.
var gm = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(
		html.WithXHTML(),
	),
)

// sanitizer strips any tag or attribute a rendered body must not carry — script,
// style, event handlers, form controls, unsafe URL schemes — leaving inert
// structural markup only. It is Cairn's second sanitization layer over
// goldmark's raw-HTML omission, so a body that contains `<script>` or an
// event-handler attribute cannot execute in the app origin (SPEC-0003 Security
// REQ "Untrusted markdown body"). We start from the UGC policy (a conservative
// allow-list for user-generated content) and add the code-fence language class
// so a later syntax pass can style fenced blocks.
var sanitizer = newSanitizer()

func newSanitizer() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Preserve the code-fence language hint goldmark emits (class="language-go");
	// it is inert and lets CSS/JS style fenced blocks by language.
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "span", "pre")
	// Add rel=noopener to any target=_blank the UGC policy allows, and force
	// external links to open safely.
	p.RequireNoReferrerOnLinks(true)
	return p
}

// TOCEntry is one heading in the table of contents. Children are the headings
// nested one level deeper, so the TOC renders as a real multi-level tree
// (SPEC-0003 issue #18: "collapsible multi-level CONTENTS TOC"). Href is the
// in-page link to the heading's block (its deterministic block_id), so TOC
// entries link to their heading blocks (SPEC-0003 REQ "Markdown Viewer").
type TOCEntry struct {
	Level    int
	Text     string
	BlockID  string
	Children []TOCEntry
}

// Block is one rendered top-level markdown block: its deterministic id, the
// sanitized HTML of its content, and whether it is a bullet/ordered list (so the
// viewer can offer the md_bullet affordance on its items). HTML is trusted —
// already goldmark-rendered and bluemonday-sanitized — so the template emits it
// verbatim.
type Block struct {
	BlockID string
	HTML    template.HTML
	IsList  bool
	Heading int // heading level (1-6), or 0 for a non-heading block
}

// Stats are the derived facts the metadata panel's DETAILS section shows
// (SPEC-0003 issue #18: "DETAILS facts").
type Stats struct {
	Words    int
	Headings int
	Blocks   int
	ReadMins int
}

// Rendered is the full result of rendering a markdown body: the ordered blocks,
// the table of contents, the derived stats, and the canonical source text (so
// the client can compute text_selection offsets against the canonical body).
type Rendered struct {
	Blocks []Block
	TOC    []TOCEntry
	Stats  Stats
	Source string
}

// Render converts a markdown body into its sanitized blocks, table of contents,
// and stats. Every top-level block receives a deterministic block_id (ADR-0006)
// and every heading becomes a TOC entry linking to its block. Rendering never
// fails on adversarial input — unparseable bytes degrade to an empty document
// rather than an error — but the signature returns an error to satisfy the
// BodyViewer contract and allow future strictness.
func Render(source []byte) (*Rendered, error) {
	reader := text.NewReader(source)
	doc := gm.Parser().Parse(reader)

	out := &Rendered{Source: string(source)}
	var flatHeadings []TOCEntry
	var allText strings.Builder

	position := 0
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		content := nodeText(node, source)
		allText.WriteString(content)
		allText.WriteByte(' ')

		blockID := BlockID(position, content)

		var buf bytes.Buffer
		if err := gm.Renderer().Render(&buf, source, node); err != nil {
			return nil, err
		}
		safe := sanitizer.SanitizeBytes(buf.Bytes())

		blk := Block{
			BlockID: blockID,
			HTML:    template.HTML(safe), //nolint:gosec // sanitized above
		}
		switch n := node.(type) {
		case *ast.List:
			blk.IsList = true
		case *ast.Heading:
			blk.Heading = n.Level
			flatHeadings = append(flatHeadings, TOCEntry{
				Level:   n.Level,
				Text:    strings.TrimSpace(content),
				BlockID: blockID,
			})
		}
		out.Blocks = append(out.Blocks, blk)
		position++
	}

	out.TOC = nestTOC(flatHeadings)
	out.Stats = deriveStats(allText.String(), len(flatHeadings), len(out.Blocks))
	return out, nil
}

// nestTOC turns a flat, document-ordered list of headings into a multi-level
// tree by heading level, so a `##` under a `#` becomes its child. Levels that
// skip (a `###` directly under a `#`) attach to the nearest shallower heading,
// which keeps the tree well-formed for any real document.
func nestTOC(flat []TOCEntry) []TOCEntry {
	var roots []TOCEntry
	// stack holds pointers into the growing tree by depth.
	var stack []*TOCEntry
	for _, h := range flat {
		entry := h
		// Pop until the top of the stack is a strictly shallower heading.
		for len(stack) > 0 && stack[len(stack)-1].Level >= entry.Level {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, entry)
			stack = append(stack, &roots[len(roots)-1])
			continue
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, entry)
		stack = append(stack, &parent.Children[len(parent.Children)-1])
	}
	return roots
}

// deriveStats computes the DETAILS facts from the document's plain text and
// structural counts. Reading time uses 200 words/minute, rounded up to at least
// one minute for any non-empty document.
func deriveStats(plain string, headings, blocks int) Stats {
	words := len(strings.Fields(plain))
	mins := 0
	if words > 0 {
		mins = (words + 199) / 200
	}
	return Stats{Words: words, Headings: headings, Blocks: blocks, ReadMins: mins}
}

// nodeText extracts a block's plain text deterministically for both its block id
// content and the document's word count. It walks the node collecting inline
// text and code-fence lines (which are not inline Text nodes), so the same body
// always yields the same text — and therefore the same block id — on every
// render (ADR-0006).
func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := node.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(source))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *ast.AutoLink:
			b.Write(t.URL(source))
		case *ast.FencedCodeBlock:
			writeLines(&b, t.Lines(), source)
		case *ast.CodeBlock:
			writeLines(&b, t.Lines(), source)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

func writeLines(b *strings.Builder, lines *text.Segments, source []byte) {
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
}
