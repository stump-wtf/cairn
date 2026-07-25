package code

import (
	"regexp"
	"strings"
)

// Symbol is one entry in the navigable outline: a named declaration and the
// 1-based source line it starts on, so an outline entry can link straight to
// its line anchor (SPEC-0003 REQ "Code Viewer": "a navigable list of
// top-level symbols that jump to their line").
type Symbol struct {
	Name string
	Kind string
	Line int
}

// declRe is the generic cross-language heuristic: a (possibly indented, so
// methods and nested declarations are picked up too — "top-level" here means
// "a named thing worth jumping to", per the issue's own examples of
// "funcs/classes/methods") declaration keyword, optionally preceded by
// visibility/modifier keywords, followed by an identifier. It intentionally
// covers the union of keywords across the languages Cairn expects to see
// (Go, Python, JS/TS, Ruby, Rust, Java, C/C++/C#, PHP, Kotlin, Swift) rather
// than hand-maintaining one regex per language, which is the "per-language
// heuristic pass" SPEC-0003 calls out as sufficient. Go's regexp package is
// RE2 (no lookahead/backreferences), so false positives (e.g. a control-flow
// keyword that happens to precede parens) are filtered by the caller instead
// of by the pattern.
var declRe = regexp.MustCompile(
	`^\s*(?:(?:pub(?:\([^)]*\))?|public|private|protected|internal|static|final|abstract|export|default|async|virtual|override|sealed|open|readonly|unsafe)\s+)*` +
		`(class|struct|interface|enum|trait|module|namespace|protocol|record|object|def|fn|func|function|fun|type)\s+` +
		`([A-Za-z_][\w.:$]*)`)

// goMethodRe catches a Go method's receiver form, `func (r *Foo) Bar(...)`,
// which declRe's keyword-then-identifier shape cannot match (the receiver
// parenthetical sits between "func" and the method name).
var goMethodRe = regexp.MustCompile(`^func\s*\([^)]*\)\s*([A-Za-z_]\w*)\s*\(`)

// arrowFuncRe catches a JS/TS function assigned to a const/let/var via an
// arrow expression, `const foo = (…) => …` or `export const foo = async () =>
// …`, a very common top-level function shape declRe's keyword list (which has
// no "const"/"let"/"var" entry — those are far too generic to add
// unconditionally) does not cover.
var arrowFuncRe = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?\(?[^=;]*\)?\s*=>`)

// cFuncRe catches a C/C++ function definition — a return type, the function
// name, a parameter list, and an opening brace on the same line — which has
// no declaration keyword at all. wordStoplist below filters the control-flow
// statements ("if (…) {", "for (…) {", …) that also match this shape.
var cFuncRe = regexp.MustCompile(`^[A-Za-z_][\w:<>,\*&\s]*[\s\*&]([A-Za-z_]\w*)\s*\([^;]*\)\s*\{?\s*$`)

// wordStoplist rejects cFuncRe matches on a C-family control-flow statement,
// which shares the "identifier(...)" shape with a function call/definition.
var wordStoplist = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"else": true, "catch": true, "sizeof": true, "do": true, "typedef": true,
}

// declKindOverride reclassifies a generic "type" match by what follows it on
// the line, so Go's `type Foo struct {` reports as "struct" and `type Foo
// interface {` as "interface" rather than the less useful "type".
var declKindOverride = regexp.MustCompile(`\bstruct\b|\binterface\b`)
var declKindStruct = regexp.MustCompile(`\bstruct\b`)

// Outline derives the navigable symbol list for a code body using a
// per-language heuristic pass (SPEC-0003 REQ "Code Viewer"): declKeyRe plus
// (for the languages it cannot fully cover) a small set of shape-specific
// extra passes, applied line by line. language is the lowercased chroma
// lexer name (Language.Key); unrecognized languages still get the generic
// pass, which is deliberately keyword-driven so it degrades gracefully rather
// than finding nothing.
func Outline(source, language string) []Symbol {
	lines := splitLines(source)
	seen := map[[2]any]bool{} // dedupe by (line, name) across passes
	var out []Symbol

	add := func(name, kind string, line int) {
		// declRe's name class allows a trailing ":" (for Ruby's "::"
		// namespacing, e.g. "Foo::Bar") so it also swallows Python's
		// block-opening colon ("class Foo:") into the captured name; strip
		// it rather than exclude ":" from the class and lose the Ruby case.
		name = strings.TrimRight(name, ":")
		if name == "" || wordStoplist[name] {
			return
		}
		key := [2]any{line, name}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Symbol{Name: name, Kind: kind, Line: line})
	}

	for i, line := range lines {
		lineNum := i + 1
		if m := declRe.FindStringSubmatch(line); m != nil {
			kind, name := m[1], m[2]
			if kind == "type" && declKindOverride.MatchString(line) {
				if declKindStruct.MatchString(line) {
					kind = "struct"
				} else {
					kind = "interface"
				}
			}
			add(name, kind, lineNum)
			continue
		}
		switch language {
		case "go":
			if m := goMethodRe.FindStringSubmatch(line); m != nil {
				add(m[1], "method", lineNum)
			}
		case "javascript", "typescript", "jsx", "tsx":
			if m := arrowFuncRe.FindStringSubmatch(line); m != nil {
				add(m[1], "const", lineNum)
			}
		case "c", "c++", "objective-c", "objective-c++":
			if m := cFuncRe.FindStringSubmatch(line); m != nil {
				add(m[1], "func", lineNum)
			}
		}
	}
	return out
}

// splitLines splits on "\n" and trims a trailing "\r", matching how
// SplitTokensIntoLines in render.go breaks the same source — so a symbol's
// Line always agrees with the highlighted Line.Num it links to.
func splitLines(source string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(source); i++ {
		if source[i] == '\n' {
			lines = append(lines, trimCR(source[start:i]))
			start = i + 1
		}
	}
	if start < len(source) {
		lines = append(lines, trimCR(source[start:]))
	}
	return lines
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
