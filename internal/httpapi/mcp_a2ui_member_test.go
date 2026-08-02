package httpapi

import (
	"strings"
	"testing"
)

// TestA2UIMemberURIRoundTrip proves a2uiMemberURIEscape and matchA2UIMemberURI
// are inverses through a full member URI, for the awkward names members
// actually carry: nested paths, spaces, percent signs, '+' and non-ASCII.
func TestA2UIMemberURIRoundTrip(t *testing.T) {
	for _, name := range []string{
		"README.md",
		"dir/file.md",
		"deep/dir/file.md",
		"a b.md",
		"100%.md",
		"a+b.md",
		"naïve.md",
	} {
		for _, scheme := range []string{"cairn://", "mcp://cairn/"} {
			uri := scheme + "bundle/abc123/" + a2uiMemberURIEscape(name) + "/a2ui"
			id, got, ok := matchA2UIMemberURI(uri)
			if !ok || id != "abc123" || got != name {
				t.Errorf("round-trip %q via %q = (%q, %q, %v), want (abc123, %q, true)",
					name, uri, id, got, ok, name)
			}
		}
	}
}

// TestMatchA2UIMemberURI pins the accepted and rejected URI shapes: both
// schemes, the stripped ?w= hint, and every malformed shape that must not
// reach the store.
func TestMatchA2UIMemberURI(t *testing.T) {
	for _, tc := range []struct {
		uri      string
		wantID   string
		wantName string
		wantOK   bool
	}{
		{"cairn://bundle/abc/README.md/a2ui", "abc", "README.md", true},
		{"mcp://cairn/bundle/abc/README.md/a2ui", "abc", "README.md", true},
		{"cairn://bundle/abc/README.md/a2ui?w=120", "abc", "README.md", true},
		{"mcp://cairn/bundle/abc/dir%2Fmain.go/a2ui?w=80", "abc", "dir/main.go", true},
		{"cairn://bundle/abc/a2ui/a2ui", "abc", "a2ui", true}, // a member literally named a2ui
		{"cairn://bundle/abc/a2ui", "", "", false},            // the bundle surface, not a member
		{"cairn://bundle/abc/a/b/a2ui", "", "", false},        // literal "/" never matches {name}
		{"cairn://bundle/abc//a2ui", "", "", false},           // empty name segment
		{"cairn://bundle//README.md/a2ui", "", "", false},     // empty id
		{"cairn://bundle/abc/README.md", "", "", false},       // missing /a2ui suffix
		{"cairn://bundle/abc/%zz/a2ui", "", "", false},        // bad percent-escape
		{"cairn://artifact/abc/README.md/a2ui", "", "", false},
		{"https://bundle/abc/README.md/a2ui", "", "", false},
	} {
		id, name, ok := matchA2UIMemberURI(tc.uri)
		if id != tc.wantID || name != tc.wantName || ok != tc.wantOK {
			t.Errorf("matchA2UIMemberURI(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.uri, id, name, ok, tc.wantID, tc.wantName, tc.wantOK)
		}
	}
}

// TestA2UIMemberIDSanitize pins the slug rules: runs of non-alphanumerics
// collapse to one dash, with no leading or trailing dash.
func TestA2UIMemberIDSanitize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"README.md", "README-md"},
		{"dir/file.md", "dir-file-md"},
		{"a...b", "a-b"},
		{".hidden", "hidden"},
		{"trailing.", "trailing"},
		{"!!!", ""},
		{"naïve.md", "na-ve-md"},
	} {
		if got := a2uiMemberIDSanitize(tc.in); got != tc.want {
			t.Errorf("a2uiMemberIDSanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestA2UIMemberSurfaceIDDistinct proves surface identity survives slug
// collisions: member names that differ only in punctuation sanitize to the
// same slug but must still name different surfaces, and every member surface
// is distinct from its bundle's list surface.
func TestA2UIMemberSurfaceIDDistinct(t *testing.T) {
	const bundle = "abc123"
	a := a2uiMemberSurfaceID(bundle, "a.b.md")
	b := a2uiMemberSurfaceID(bundle, "a-b.md")
	if a == b {
		t.Errorf("surface ids collide for punctuation-only name difference: %q", a)
	}
	if !strings.HasPrefix(a, a2uiBundleSurfaceID(bundle)+"-member-") {
		t.Errorf("member surface id %q does not extend the bundle surface id", a)
	}
	if a == a2uiBundleSurfaceID(bundle) {
		t.Error("member surface id equals the bundle surface id")
	}
	// Deterministic: the same member always names the same surface.
	if again := a2uiMemberSurfaceID(bundle, "a.b.md"); again != a {
		t.Errorf("surface id not stable: %q then %q", a, again)
	}
}
