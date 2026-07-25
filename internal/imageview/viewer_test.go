package imageview

import (
	"strings"
	"testing"

	"github.com/joestump/cairn/internal/artifact"
)

// TestFragmentStructure asserts the rendered fragment carries the wiring
// image.js hooks into (SPEC-0003 REQ "Image Viewer"): the pin-drop frame with
// its <img> pointed at the inline body route, an initially-empty pin overlay
// layer, and the react-below toolbar with its "+" trigger.
func TestFragmentStructure(t *testing.T) {
	a := &artifact.Artifact{PublicID: "abc123", Title: "roadmap.png"}
	html, err := Fragment(a)
	if err != nil {
		t.Fatalf("Fragment: %v", err)
	}
	s := string(html)
	for _, frag := range []string{
		`data-img-viewer`,
		`data-artifact-id="abc123"`,
		`data-img-frame`,
		`tabindex="0"`,
		`src="/abc123/image"`,
		`alt="roadmap.png"`,
		`data-img-pins`,
		`data-img-react-cluster`,
		`data-img-react-add`,
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("fragment missing %q:\n%s", frag, s)
		}
	}
}

// TestFragmentAltFallsBackToPublicID asserts an untitled artifact still gets a
// non-empty accessible name on the <img> (WCAG 1.1.1).
func TestFragmentAltFallsBackToPublicID(t *testing.T) {
	a := &artifact.Artifact{PublicID: "xyz789"}
	html, err := Fragment(a)
	if err != nil {
		t.Fatalf("Fragment: %v", err)
	}
	if !strings.Contains(string(html), `alt="xyz789"`) {
		t.Errorf("fragment should fall back to the public id for alt text:\n%s", html)
	}
}

// TestFragmentEscapesTitle asserts a hostile title cannot inject markup into
// the alt attribute or break out of it (XSS: user content escaped, never
// executed).
func TestFragmentEscapesTitle(t *testing.T) {
	a := &artifact.Artifact{PublicID: "abc123", Title: `"><script>alert(1)</script>`}
	html, err := Fragment(a)
	if err != nil {
		t.Fatalf("Fragment: %v", err)
	}
	if strings.Contains(string(html), "<script>") {
		t.Errorf("fragment leaked unescaped script tag:\n%s", html)
	}
}
