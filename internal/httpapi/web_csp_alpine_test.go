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
	// Any x-on/x-show/x-bind/x-text/x-data/x-init/x-effect/@event directive
	// whose expression contains "(" is not resolvable by the CSP build.
	//
	// The leading `[\s"]` requires the directive name to start right after
	// attribute-boundary whitespace (or, defensively, a quote) so "x-on:" does
	// not also match inside an unrelated attribute like hx-on:click — htmx's
	// own event-binding attribute, which legitimately carries a JS call
	// expression and is not subject to the Alpine CSP build's restrictions.
	// The value alternation covers both double- and single-quoted attributes.
	re := regexp.MustCompile(`[\s"](x-(?:on:[a-z.-]+|show|text|bind:[a-zA-Z-]+|data|init|effect)|@[a-z.-]+)=(?:"[^"]*\(|'[^']*\()`)

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

// TestAlpineCSPRegexCases pins the detector's behavior directly against the
// false positive and the new directive/quote coverage from PR #41 review
// note 2, independent of whatever the current templates happen to contain.
func TestAlpineCSPRegexCases(t *testing.T) {
	re := regexp.MustCompile(`[\s"](x-(?:on:[a-z.-]+|show|text|bind:[a-zA-Z-]+|data|init|effect)|@[a-z.-]+)=(?:"[^"]*\(|'[^']*\()`)

	cases := []struct {
		name  string
		frag  string
		match bool
	}{
		{"bare identifier is fine", `<button x-on:click="copy">`, false},
		{"call expression is caught", `<button x-on:click="copy()">`, true},
		{"htmx hx-on is not a false positive", `<button hx-on:click="doThing()">`, false},
		{"x-show call expression", `<span x-show="isOpen()">`, true},
		{"x-data call expression", `<body x-data="shell()">`, true},
		{"x-data bare identifier is fine", `<body x-data="shell">`, false},
		{"x-init call expression", `<body x-init="load()">`, true},
		{"x-effect call expression", `<div x-effect="sync()">`, true},
		{"@event call expression", `<button @click="close()">`, true},
		{"single-quoted call expression", `<button x-on:click='copy()'>`, true},
		{"single-quoted bare identifier is fine", `<button x-on:click='copy'>`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := re.MatchString(c.frag)
			if got != c.match {
				t.Errorf("MatchString(%q) = %v, want %v", c.frag, got, c.match)
			}
		})
	}
}
