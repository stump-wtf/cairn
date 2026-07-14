package clicmd

import (
	"net/http"
	"path/filepath"
	"strings"
)

// extMediaTypes covers common share-worthy extensions the CLI wants to get
// right without relying on the host OS's mime.types database (which varies
// wildly between minimal containers, macOS, and Linux distros) — SPEC-0008
// "media-type detection (md by content/extension hint flag)". The server
// remains free to re-sniff (SPEC-0008 "the server may re-sniff it"); this is
// only a client-declared hint.
var extMediaTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".log":      "text/plain",
	".csv":      "text/csv",
	".tsv":      "text/tab-separated-values",
	".json":     "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".xml":      "application/xml",
	".html":     "text/html",
	".htm":      "text/html",
	".css":      "text/css",
	".sql":      "application/sql",
	".sh":       "text/x-shellscript",
	".bash":     "text/x-shellscript",
	".zsh":      "text/x-shellscript",
	".py":       "text/x-python",
	".go":       "text/x-go",
	".rs":       "text/x-rust",
	".js":       "text/javascript",
	".mjs":      "text/javascript",
	".ts":       "text/x-typescript",
	".tsx":      "text/x-typescript",
	".jsx":      "text/javascript",
	".java":     "text/x-java",
	".c":        "text/x-c",
	".h":        "text/x-c",
	".cpp":      "text/x-c++",
	".rb":       "text/x-ruby",
	".php":      "text/x-php",
	".diff":     "text/x-diff",
	".patch":    "text/x-diff",
	".pdf":      "application/pdf",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".svg":      "image/svg+xml",
}

// detectMediaType resolves the Content-Type the CLI declares for a create
// request (SPEC-0008 "media-type detection"), in priority order: an explicit
// --type override, an extension lookup against path (when a path argument
// was given), then a content sniff of peek (the first bytes read from
// stdin), falling back to the generic octet-stream type the server treats
// as "sniff me." The server is always free to override this via its own
// media-type detection — the CLI's job is a best-effort hint, never a
// binding decision (SPEC-0008 "Data the CLI shows but never decides").
func detectMediaType(override, path string, peek []byte) string {
	if override != "" {
		return override
	}
	if path != "" {
		if mt, ok := extMediaTypes[strings.ToLower(filepath.Ext(path))]; ok {
			return mt
		}
	}
	if len(peek) > 0 {
		return http.DetectContentType(peek)
	}
	return "application/octet-stream"
}
