// Command gen-chroma-css regenerates the code viewer's bundled syntax-theme
// stylesheet (internal/httpapi/web/assets/chroma.css) from chroma's built-in
// "github-dark" style, using the exact same formatter options
// internal/code/render.go highlights with (WithClasses, ClassPrefix
// "chroma-", PreventSurroundingPre) so the class names the viewer emits and
// the class names this stylesheet defines can never drift apart.
//
// The code viewer highlights under a strict `style-src 'self'` CSP with no
// inline style attributes and no external stylesheet (SPEC-0003, ADR-0011),
// so the theme must ship as a static, embedded asset rather than being
// generated per-request — this command is that one-time (well, one-per-theme-
// change) generation step, not something the server runs.
//
// Usage: go run ./cmd/gen-chroma-css > internal/httpapi/web/assets/chroma.css
package main

import (
	"bufio"
	"fmt"
	"os"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
)

const header = `/* Chroma syntax-highlighting theme, generated from chroma's built-in
   "github-dark" style (github.com/alecthomas/chroma/v2/styles) and bundled
   here as a static asset so the code viewer can highlight source under a
   strict style-src 'self' CSP with no inline style attributes and no
   external stylesheet (SPEC-0003, ADR-0011).

   Regenerate with:
     go run ./cmd/gen-chroma-css > internal/httpapi/web/assets/chroma.css

   DO NOT hand-edit below this line — edit this generator instead. */

`

func main() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	if _, err := fmt.Fprint(w, header); err != nil {
		fail(err)
	}
	formatter := chromahtml.New(
		chromahtml.WithClasses(true),
		chromahtml.ClassPrefix("chroma-"),
		chromahtml.PreventSurroundingPre(true),
	)
	style := styles.Get("github-dark")
	if err := formatter.WriteCSS(w, style); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen-chroma-css:", err)
	os.Exit(1)
}
