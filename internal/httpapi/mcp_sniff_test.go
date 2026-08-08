package httpapi

import (
	"strings"
	"testing"
)

func TestSniffMarkdown(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "Hello world, this is just plain text.", ""},
		{"heading only", "# Title", ""},
		{"heading + list", "# Title\n\n- item one\n- item two", "text/markdown"},
		{"heading + bold", "# Title\n\nThis has **bold** text.", "text/markdown"},
		{"code fence + heading", "# Docs\n\n```go\nfmt.Println()\n```", "text/markdown"},
		{"link + heading", "# Links\n\nSee [docs](https://example.com).", "text/markdown"},
		{"list + bold", "- **item one**\n- item two", "text/markdown"},
		{"mid-body heading", "Some intro text.\n\n## Section\n\nMore text.", "text/markdown"},
		{"just bold", "This has **bold** but nothing else.", ""},
		{"long body with markdown", "# Title\n\n" + strings.Repeat("Some paragraph text. ", 10) + "\n- item\n- another", "text/markdown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sniffMarkdown(tt.body)
			if got != tt.want {
				t.Errorf("sniffMarkdown() = %q, want %q", got, tt.want)
			}
		})
	}
}
