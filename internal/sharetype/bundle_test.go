package sharetype

import (
	"testing"

	"github.com/joestump/cairn/internal/artifact"
)

// TestBundleComposedViewer asserts the bundle type carries the ComposedViewer
// capability (so httpapi picks the member-rail path off registry data, not a
// type-key switch) and that no single-body type does — resolution stays total.
func TestBundleComposedViewer(t *testing.T) {
	r := Default()
	cv, ok := r.ComposedViewerFor(artifact.TypeBundle)
	if !ok {
		t.Fatal("bundle must implement ComposedViewer")
	}
	if cv.MemberAnchor() != AnchorBundleFile {
		t.Fatalf("bundle MemberAnchor = %q, want %q", cv.MemberAnchor(), AnchorBundleFile)
	}
	for _, key := range []artifact.ShareType{KeyMarkdown, KeyCode, artifact.TypeFile, KeyTrajectory} {
		if _, ok := r.ComposedViewerFor(key); ok {
			t.Errorf("type %q must NOT implement ComposedViewer (single-body/bodyless)", key)
		}
	}
}

// TestClassifyMember resolves a bundle member's own share type from its name +
// sniffed media type: markdown by extension (ingest sniffs .md as text/plain, so
// the extension is the signal) or an explicit markdown media type, and the
// generic file floor for everything else (SPEC-0003 "delegate the member back
// through the registry").
func TestClassifyMember(t *testing.T) {
	r := Default()
	cases := []struct {
		name, media string
		want        artifact.ShareType
	}{
		{"notes.md", "text/plain; charset=utf-8", KeyMarkdown},
		{"README.markdown", "text/plain", KeyMarkdown},
		{"doc", "text/markdown", KeyMarkdown},
		{"data.bin", "application/octet-stream", artifact.TypeFile},
		{"dump.sql.gz", "application/gzip", artifact.TypeGZ},
		{"photo.png", "image/png", artifact.TypeFile}, // no image member viewer yet in 0.0.2
		{"script.go", "text/plain", artifact.TypeFile},
	}
	for _, tc := range cases {
		if got := r.ClassifyMember(tc.name, tc.media); got != tc.want {
			t.Errorf("ClassifyMember(%q,%q) = %q, want %q", tc.name, tc.media, got, tc.want)
		}
	}
}
