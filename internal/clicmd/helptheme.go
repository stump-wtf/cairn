package clicmd

import (
	"image/color"

	fang "charm.land/fang/v2"
	"charm.land/lipgloss/v2"
)

// CairnColorScheme re-skins fang's help/error layout — the same surface
// Crush renders, via charm.land/fang/v2 — with the Cairn design language.
//
// Every hex below is copied verbatim from website/src/css/custom.css, the
// site's token file and the only sanctioned home for a Cairn color literal
// (ADR-0014, SPEC-0010 REQ "Design Token Source of Truth").
// helptheme_test.go fails the build if any hex here drifts out of that
// file, mirroring what `npm run lint:tokens` enforces for the website.
//
// The category accents are dark-ground text colours by design (SPEC-0010
// REQ "Category Accents Are Dark-Ground Text Colours") — exactly what a
// terminal help screen is — so they map onto fang's roles unchanged in
// dark mode. Light mode substitutes the ink ramp and the darker steps of
// the primary ladder where an accent would not hold on a white ground.
func CairnColorScheme(c lipgloss.LightDarkFunc) fang.ColorScheme {
	// fang hands us lipgloss.LightDark(light, dark) — note the order.
	ground := func(dark, light string) color.Color {
		return c(lipgloss.Color(light), lipgloss.Color(dark))
	}
	return fang.ColorScheme{
		// Body text and flag/command descriptions: the ink ramp's
		// near-white on dark terminals, near-black ink on light ones.
		Base:        ground("#e7e8ea", "#08090b"), // --cairn-ink-900 / --cairn-ink-deep
		Description: ground("#e7e8ea", "#08090b"),
		Argument:    ground("#e7e8ea", "#08090b"),
		Help:        ground("#e7e8ea", "#08090b"),

		// Section titles and the program name: the link/accent lavender
		// that holds 4.5:1 across the ink ramp, or the brand purple on
		// light grounds.
		Title:   ground("#b5abf9", "#6b4ce6"), // --cairn-ink-accent / --ifm-color-primary
		Program: ground("#b5abf9", "#6b4ce6"),

		// Usage/example codeblock background: the pinned machine-content
		// surface and its light twin.
		Codeblock: ground("#131418", "#eeeef2"), // --cairn-ink-100 / --cairn-paper-200

		// Subcommands: read blue; the darkest step of the primary ladder
		// keeps the hue legible on white.
		Command: ground("#4f9cf9", "#4f39ef"), // --cat-read / --ifm-color-primary-darkest

		// Flags: exec green in both modes — fang's own default flag color
		// is green too, so this preserves the Crush feel.
		Flag: ground("#46c878", "#46c878"), // --cat-exec

		// Quoted strings in usage lines: write pink (fang's default is the
		// same coral family).
		QuotedString: ground("#f0568f", "#f0568f"), // --cat-write

		// Dimmed machinery — flag defaults, comments, dashes: meta slate,
		// stepping down the paper ramp on light grounds.
		DimmedArgument: ground("#808c9a", "#5c626c"), // --cat-meta / --cairn-paper-700
		Comment:        ground("#808c9a", "#5c626c"),
		FlagDefault:    ground("#808c9a", "#5c626c"),
		Dash:           ground("#808c9a", "#5c626c"),

		// Errors: an "ERROR" badge of paper-white on fail red, fail-red
		// details underneath.
		ErrorHeader: [2]color.Color{ // fg --cairn-paper-100, bg --cat-fail
			lipgloss.Color("#f6f6f8"),
			lipgloss.Color("#f0506a"),
		},
		ErrorDetails: ground("#f0506a", "#f0506a"), // --cat-fail
	}
}
