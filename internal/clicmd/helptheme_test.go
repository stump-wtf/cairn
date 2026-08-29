package clicmd

import (
	"image/color"
	"os"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestHelpThemeHexesComeFromCustomCSS is the CLI-side mirror of the
// website's `npm run lint:tokens`: every color literal in helptheme.go
// must appear verbatim in website/src/css/custom.css, the Cairn design
// language's single token file (ADR-0014, SPEC-0010 REQ "Design Token
// Source of Truth"). A hex that exists only here is drift — someone
// re-skinned the CLI by eye — and fails the build exactly as a stray
// literal in the website would.
func TestHelpThemeHexesComeFromCustomCSS(t *testing.T) {
	goSrc, err := os.ReadFile("helptheme.go")
	if err != nil {
		t.Fatalf("read helptheme.go: %v", err)
	}
	cssSrc, err := os.ReadFile("../../website/src/css/custom.css")
	if err != nil {
		t.Fatalf("read website/src/css/custom.css: %v", err)
	}
	css := string(cssSrc)

	hexes := regexp.MustCompile(`#[0-9a-fA-F]{6}\b`).FindAllString(string(goSrc), -1)
	if len(hexes) == 0 {
		t.Fatal("helptheme.go contains no hex literals — has the palette been deleted, or did the test's file path rot?")
	}
	for _, hex := range hexes {
		if !strings.Contains(css, hex) {
			t.Errorf("helptheme.go uses %s, which does not appear in website/src/css/custom.css; the Cairn palette is the only color source", hex)
		}
	}
}

// TestCairnColorSchemeSetsEveryRole guards against a partially-filled
// scheme silently falling back to fang's Charmtone defaults: every fang
// color role must be non-nil in both terminal modes.
func TestCairnColorSchemeSetsEveryRole(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		scheme := CairnColorScheme(lipgloss.LightDark(isDark))
		roles := map[string]color.Color{
			"Base":           scheme.Base,
			"Title":          scheme.Title,
			"Description":    scheme.Description,
			"Codeblock":      scheme.Codeblock,
			"Program":        scheme.Program,
			"DimmedArgument": scheme.DimmedArgument,
			"Comment":        scheme.Comment,
			"Flag":           scheme.Flag,
			"FlagDefault":    scheme.FlagDefault,
			"Command":        scheme.Command,
			"QuotedString":   scheme.QuotedString,
			"Argument":       scheme.Argument,
			"Help":           scheme.Help,
			"Dash":           scheme.Dash,
			"ErrorDetails":   scheme.ErrorDetails,
		}
		for name, c := range roles {
			if c == nil {
				t.Errorf("CairnColorScheme(isDark=%v) leaves role %s nil — fang would render it in Charmtone defaults", isDark, name)
			}
		}
		for i, c := range scheme.ErrorHeader {
			if c == nil {
				t.Errorf("CairnColorScheme(isDark=%v) leaves ErrorHeader[%d] nil", isDark, i)
			}
		}
	}
}
