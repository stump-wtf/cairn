package code

import (
	"strings"
	"testing"
)

const goSample = `package sample

import "fmt"

// Greet prints a greeting.
func Greet(name string) string {
	return "hello " + name
}

type Server struct {
	Name string
}

func (s *Server) Start() error {
	fmt.Println(s.Name)
	return nil
}

type Handler interface {
	Handle()
}
`

func TestRenderProducesOneLinePerSourceLine(t *testing.T) {
	r, err := Render([]byte(goSample), "text/x-go", "sample.go", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := len(strings.Split(goSample, "\n")) - 1 // trailing newline yields no extra line
	if r.LineCount != want {
		t.Fatalf("LineCount = %d, want %d", r.LineCount, want)
	}
	if len(r.Lines) != r.LineCount {
		t.Fatalf("len(Lines) = %d, want %d", len(r.Lines), r.LineCount)
	}
	for i, ln := range r.Lines {
		if ln.Num != i+1 {
			t.Errorf("Lines[%d].Num = %d, want %d", i, ln.Num, i+1)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	a, err := Render([]byte(goSample), "text/x-go", "sample.go", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b, err := Render([]byte(goSample), "text/x-go", "sample.go", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(a.Lines) != len(b.Lines) {
		t.Fatalf("line counts differ: %d vs %d", len(a.Lines), len(b.Lines))
	}
	for i := range a.Lines {
		if a.Lines[i].HTML != b.Lines[i].HTML {
			t.Errorf("line %d HTML drifted between renders", i+1)
		}
	}
}

// TestRenderEscapesUntrustedContent asserts a source body containing HTML/JS
// is rendered as inert, escaped text — never executable markup (SPEC-0003
// Security Requirements: "source is escaped, never executed").
func TestRenderEscapesUntrustedContent(t *testing.T) {
	src := "<script>alert(1)</script>\n// <img src=x onerror=alert(1)>\n"
	r, err := Render([]byte(src), "text/plain", "notes.txt", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, ln := range r.Lines {
		html := string(ln.HTML)
		if strings.Contains(html, "<script") || strings.Contains(html, "<img ") {
			t.Fatalf("line %d HTML carries an unescaped tag: %s", ln.Num, html)
		}
		if strings.Contains(html, "<") && !strings.Contains(html, "<span") {
			t.Fatalf("line %d HTML carries an unexpected raw '<': %s", ln.Num, html)
		}
	}
}

func TestRenderEmptyBody(t *testing.T) {
	r, err := Render([]byte(""), "text/plain", "empty.txt", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.LineCount != 0 || len(r.Lines) != 0 {
		t.Fatalf("empty body should render zero lines, got %d", r.LineCount)
	}
}

func TestRenderNeverErrorsOnAdversarialInput(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x00, 0x01, 0xff, 0xfe},
		[]byte(strings.Repeat("a", 1<<16)),
		[]byte("\xc3\x28"), // invalid UTF-8
	}
	for _, c := range cases {
		if _, err := Render(c, "application/octet-stream", "blob.bin", ""); err != nil {
			t.Errorf("Render(%d bytes) returned error, want graceful degradation: %v", len(c), err)
		}
	}
}

func TestLanguageDetection(t *testing.T) {
	cases := []struct {
		name                       string
		mediaType, title, override string
		wantKey                    string
	}{
		{"extension wins over generic media type", "text/plain", "main.go", "", "go"},
		{"media type used when title has no extension", "text/x-python", "", "", "python"},
		{"override wins over everything", "text/x-go", "main.go", "python3", "python"},
		{"unknown falls back to plaintext", "application/octet-stream", "weird.zzzzz", "", "text"},
		{"json by extension", "text/plain", "config.json", "", "json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lang := Detect(tc.override, tc.mediaType, tc.title)
			if lang.Key != tc.wantKey {
				t.Errorf("Detect(%q, %q, %q).Key = %q, want %q", tc.override, tc.mediaType, tc.title, lang.Key, tc.wantKey)
			}
		})
	}
}

func TestRenderMultilineTokenSplitsAcrossLines(t *testing.T) {
	src := "s := `line one\nline two\nline three`\n"
	r, err := Render([]byte(src), "text/x-go", "sample.go", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.LineCount != 3 {
		t.Fatalf("LineCount = %d, want 3", r.LineCount)
	}
	for i, ln := range r.Lines {
		if strings.Contains(string(ln.HTML), "\n") {
			t.Errorf("line %d HTML retains a literal newline: %q", i+1, ln.HTML)
		}
	}
}
