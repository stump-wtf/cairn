package code

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// Language is the resolved highlighting language for a code artifact: the
// chroma lexer that tokenises it plus the human-readable name shown in the
// STATS line and the metadata panel (SPEC-0003 REQ "Code Viewer": "supplies
// its metadata-panel fields (e.g. language, line count)").
type Language struct {
	Lexer   chroma.Lexer
	Display string
	// Key is a normalized (lowercased) form of the lexer's canonical name,
	// used to select the symbol-outline heuristic (symbols.go) — kept
	// separate from Display so a future rename of the shown label never
	// silently changes which outline heuristic runs.
	Key string
}

// genericMediaTypes carries no language signal on its own — routing a
// "text/plain" or "application/octet-stream" body through
// lexers.MatchMimeType would pick whichever lexer happens to declare that
// generic type first (observed: text/plain incorrectly resolving to the Nu
// shell lexer), so these are skipped in favor of the title's file extension.
var genericMediaTypes = map[string]bool{
	"text/plain":               true,
	"application/octet-stream": true,
	"binary/octet-stream":      true,
	"application/text":         true,
}

// Detect resolves a code artifact's highlighting language from an explicit
// override, its title's file extension, and its media type, in that
// precedence order (SPEC-0003 REQ "Code Viewer": "Language detected from
// media_type/title/extension, overridable"). The title's extension is
// checked before the media type because a filename is a deliberate,
// high-signal choice while the media type is frequently a generic
// "text/plain" that carries no language information at all. Detection never
// fails: an unrecognized combination degrades to chroma's plaintext lexer, so
// highlighting is always attempted and a code artifact always renders.
func Detect(override, mediaType, title string) Language {
	if override != "" {
		if l := lexers.Get(override); l != nil && l != lexers.Fallback {
			return languageFrom(l)
		}
	}
	if title != "" {
		if l := lexers.Match(title); l != nil && l != lexers.Fallback {
			return languageFrom(l)
		}
	}
	if mt := normalizeMediaType(mediaType); mt != "" && !genericMediaTypes[mt] {
		if l := lexers.MatchMimeType(mt); l != nil && l != lexers.Fallback {
			return languageFrom(l)
		}
	}
	return languageFrom(lexers.Fallback)
}

// normalizeMediaType strips any parameters (e.g. "; charset=utf-8") and
// lowercases the media type for lexer lookup.
func normalizeMediaType(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

func languageFrom(l chroma.Lexer) Language {
	if l == lexers.Fallback {
		// chroma's own plaintext lexer names itself "fallback", which is an
		// implementation detail, not a language a reader would recognize —
		// show "Plain Text" instead (matching how a code host's own language
		// picker reads for a body with no detected language).
		return Language{Lexer: l, Display: "Plain Text", Key: "text"}
	}
	name := l.Config().Name
	return Language{Lexer: l, Display: name, Key: strings.ToLower(name)}
}
