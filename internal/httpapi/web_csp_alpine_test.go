package httpapi

import (
	"io/fs"
	"regexp"
	"testing"
)

// The web shell ships the Alpine CSP build (@alpinejs/csp) so the HTML routes
// can keep `script-src 'self'` (ADR-0011). That build resolves directive
// expressions as bare dot-separated property paths ONLY — a call expression
// like x-on:click="copy()" fails at runtime with a console warning and a dead
// control (this exact bug shipped in 0.0.1: every header control was inert in
// Firefox/Safari/Chrome). Templates must therefore reference component members
// by bare name: x-on:click="copy", x-show="isLink".
//
// Governing: ADR-0011 (no-build frontend, CSP build), SPEC-0001 REQ
// "Progressive Enhancement".
func TestAlpineDirectivesUseBareIdentifiers(t *testing.T) {
	// Any x-on/x-show/x-bind/x-text/@event directive whose expression contains
	// "(" is not resolvable by the CSP build.
	re := regexp.MustCompile(`(x-(?:on:[a-z.-]+|show|text|bind:[a-zA-Z-]+)|@[a-z.-]+)="[^"]*\(`)

	err := fs.WalkDir(webTemplateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(webTemplateFS, path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAll(b, -1) {
			t.Errorf("%s: Alpine CSP build cannot evaluate call expressions; use a bare identifier: %s", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded web fs: %v", err)
	}
}
