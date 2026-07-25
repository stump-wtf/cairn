package clicmd

import "testing"

func TestDetectMediaTypeByExtension(t *testing.T) {
	cases := map[string]string{
		"notes.md":       "text/markdown",
		"report.MD":      "text/markdown",
		"data.json":      "application/json",
		"query.sql":      "application/sql",
		"screenshot.png": "image/png",
	}
	for path, want := range cases {
		if got := detectMediaType("", path, nil); got != want {
			t.Errorf("detectMediaType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDetectMediaTypeExplicitOverrideWins(t *testing.T) {
	got := detectMediaType("application/x-custom", "notes.md", []byte("# hi"))
	if got != "application/x-custom" {
		t.Errorf("got %q, want the override", got)
	}
}

func TestDetectMediaTypeContentSniffFallback(t *testing.T) {
	// No path (stdin) and no matching extension: falls back to content
	// sniffing on the peeked bytes.
	got := detectMediaType("", "", []byte("<html><body>hi</body></html>"))
	if got == "" || got == "application/octet-stream" {
		t.Errorf("got %q, want a sniffed HTML-ish type", got)
	}
}

func TestDetectMediaTypeDefaultsToOctetStream(t *testing.T) {
	got := detectMediaType("", "", nil)
	if got != "application/octet-stream" {
		t.Errorf("got %q, want application/octet-stream", got)
	}
}

func TestDetectMediaTypeUnknownExtensionFallsBackToSniff(t *testing.T) {
	got := detectMediaType("", "weird.zzzzz", []byte("plain text content"))
	if got == "" {
		t.Error("got empty media type")
	}
}
