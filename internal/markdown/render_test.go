package markdown

import (
	"strings"
	"testing"
)

const sampleDoc = `# Checkout Web Audit

An audit of the **checkout** flow.

## Findings

- missing CSRF token
- weak session cookie

## Remediation

Rotate the [session key](https://example.com/keys).

    plain code block
`

// TestRenderBlockIDsDeterministic asserts the same body yields the same block
// ids on two renders (ADR-0006 "Deterministic render ids", SPEC-0003 REQ
// "Markdown Viewer": "Stable block ids").
func TestRenderBlockIDsDeterministic(t *testing.T) {
	a, err := Render([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := Render([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(a.Blocks) != len(b.Blocks) || len(a.Blocks) == 0 {
		t.Fatalf("block counts differ or empty: %d vs %d", len(a.Blocks), len(b.Blocks))
	}
	for i := range a.Blocks {
		if a.Blocks[i].BlockID != b.Blocks[i].BlockID {
			t.Errorf("block %d id drifted: %q vs %q", i, a.Blocks[i].BlockID, b.Blocks[i].BlockID)
		}
		if !strings.HasPrefix(a.Blocks[i].BlockID, "b_") {
			t.Errorf("block %d id %q must carry the b_ prefix", i, a.Blocks[i].BlockID)
		}
	}
	// Distinct blocks get distinct ids.
	seen := map[string]bool{}
	for _, blk := range a.Blocks {
		if seen[blk.BlockID] {
			t.Errorf("duplicate block id %q", blk.BlockID)
		}
		seen[blk.BlockID] = true
	}
}

// TestRenderTOC asserts a multi-level TOC is derived from the headings and each
// entry links to its heading's block (SPEC-0003 REQ "Markdown Viewer").
func TestRenderTOC(t *testing.T) {
	r, err := Render([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(r.TOC) != 1 {
		t.Fatalf("want one top-level TOC root (the H1), got %d", len(r.TOC))
	}
	root := r.TOC[0]
	if root.Text != "Checkout Web Audit" || root.Level != 1 {
		t.Errorf("root TOC entry = %q L%d, want the H1", root.Text, root.Level)
	}
	if len(root.Children) != 2 {
		t.Fatalf("want two H2 children under the H1, got %d", len(root.Children))
	}
	// The TOC block id must match an actual rendered block id (in-page link
	// resolves).
	ids := map[string]bool{}
	for _, blk := range r.Blocks {
		ids[blk.BlockID] = true
	}
	if !ids[root.BlockID] {
		t.Errorf("TOC root block id %q does not match any rendered block", root.BlockID)
	}
	for _, c := range root.Children {
		if c.Level != 2 {
			t.Errorf("child %q should be level 2, got %d", c.Text, c.Level)
		}
		if !ids[c.BlockID] {
			t.Errorf("TOC child block id %q does not match any rendered block", c.BlockID)
		}
	}
}

// TestRenderStats asserts the derived DETAILS facts are sane.
func TestRenderStats(t *testing.T) {
	r, err := Render([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.Stats.Headings != 3 {
		t.Errorf("headings = %d, want 3", r.Stats.Headings)
	}
	if r.Stats.Blocks != len(r.Blocks) {
		t.Errorf("block stat %d != rendered blocks %d", r.Stats.Blocks, len(r.Blocks))
	}
	if r.Stats.Words == 0 || r.Stats.ReadMins == 0 {
		t.Errorf("expected non-zero words and read time, got %+v", r.Stats)
	}
}

// TestSanitizeStripsScript is the headline security test: an untrusted body with
// a <script> tag, an event-handler attribute, and a javascript: URL renders
// inert — no executable content survives (SPEC-0003 Security REQ "Untrusted
// markdown body").
func TestSanitizeStripsScript(t *testing.T) {
	evil := "# Title\n\n<script>alert('xss')</script>\n\n" +
		"<img src=x onerror=\"alert(1)\">\n\n" +
		"[click me](javascript:alert(2))\n\n" +
		"<a href=\"https://ok.example\" onclick=\"steal()\">safe link</a>\n"
	html, err := RenderFragment([]byte(evil))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, bad := range []string{
		"<script", "onerror", "onclick", "javascript:", "alert(",
	} {
		if strings.Contains(s, bad) {
			t.Errorf("sanitized output still contains %q:\n%s", bad, s)
		}
	}
	// The safe link's visible text and destination survive.
	if !strings.Contains(s, "safe link") {
		t.Error("sanitizer should keep the safe link text")
	}
}

// TestFragmentAffordances asserts the body fragment exposes the markdown
// annotation affordances and deterministic ids the spec requires: per-block
// wrappers carrying the block_id, an md_block react control, the TOC nav, the
// hidden canonical source, and accessible labels (SPEC-0003 REQ "Markdown
// Annotation Anchors", Accessibility REQ "Icon-Only Controls").
func TestFragmentAffordances(t *testing.T) {
	html, err := RenderFragment([]byte(sampleDoc))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, frag := range []string{
		`class="md-viewer"`,
		`aria-label="Table of contents"`,
		`<details`,
		`data-block-id="b_`,
		`data-anchor-type="md_block"`,
		`aria-label="React to this block"`,
		`class="md-prose"`,
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("fragment missing %q", frag)
		}
	}
}

// TestRenderNoHeadingsNoTOC asserts a document with no headings suppresses the
// TOC entirely rather than synthesizing an empty one (SPEC-0003 design open
// question resolved: suppress).
func TestRenderNoHeadingsNoTOC(t *testing.T) {
	r, err := Render([]byte("just a paragraph, no headings here\n"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(r.TOC) != 0 {
		t.Errorf("want no TOC for a heading-less doc, got %d entries", len(r.TOC))
	}
	html, err := Fragment(r)
	if err != nil {
		t.Fatalf("fragment: %v", err)
	}
	if strings.Contains(string(html), "md-toc") {
		t.Error("fragment should omit the TOC nav when there are no headings")
	}
}

// TestListBlocksCarryNoBlockLevelReact pins the fix for two ＋ controls landing
// on the first item of every list. markdown.js gives each list item its own
// md_bullet affordance, so rendering the block-level md_block control too put
// two triggers a few pixels apart, and the block one read as a duplicate.
// Bullet-level is the finer anchor, so lists keep only that; every other block
// keeps its block-level control.
func TestListBlocksCarryNoBlockLevelReact(t *testing.T) {
	html, err := RenderFragment([]byte("A paragraph.\n\n- one\n- two\n"))
	if err != nil {
		t.Fatalf("RenderFragment: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, `data-md-list="true"`) {
		t.Fatal("fixture did not produce a list block")
	}
	// Exactly one md_block trigger survives: the paragraph's.
	if got := strings.Count(s, `data-anchor-type="md_block"`); got != 1 {
		t.Errorf("md_block trigger count = %d, want 1 (the paragraph only; the list has per-item triggers)", got)
	}
}
