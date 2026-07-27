// Package id mints Cairn's short, opaque, public artifact identifiers.
//
// Ids are cryptographically random base62 (0-9 A-Z a-z), case-sensitive, of a
// standardized default length of 8 characters (~47.6 bits). They are minted
// independently of the body SHA-256 and the internal primary key, reveal no
// order, count, or content, and exclude reserved route words so an id can never
// collide with a route prefix. Uniqueness is finalized at write time by the
// store (generate -> atomic unique insert -> regenerate on the rare conflict).
//
// Governing: ADR-0005 (Short Opaque Identifiers and the URL Scheme),
// SPEC-0002 REQ "Short Opaque Public Identifiers and URL Scheme"
package id

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// DefaultLength is the standardized public-id length in base62 characters.
const DefaultLength = 8

// reserved route words are excluded from generated ids so a public id can never
// equal a top-level route prefix (belt-and-suspenders on ADR-0005).
var reserved = map[string]struct{}{
	"run":         {},
	"hook":        {},
	"api":         {},
	"settings":    {},
	".well-known": {},
	"v1":          {},
	"auth":        {},
	"oauth":       {},
	"static":      {},
	"assets":      {},
	"healthz":     {},
}

// New returns a random base62 id of the default length.
func New() (string, error) { return NewLength(DefaultLength) }

// NewLength returns a random base62 id of length n, retrying if the candidate
// matches a reserved route word.
func NewLength(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("id: length must be positive, got %d", n)
	}
	for {
		s, err := gen(n)
		if err != nil {
			return "", err
		}
		if IsReserved(s) {
			continue
		}
		return s, nil
	}
}

// IsReserved reports whether s equals a reserved route word.
func IsReserved(s string) bool {
	_, ok := reserved[s]
	return ok
}

// Normalize resolves a bare public id or an mcp://cairn/<id> agent handle
// (optionally under its /run/ or /hook/ sub-prefix) to the bare public id every
// core lookup takes. It is the single definition of that mapping, so the
// transport adapters and the core services cannot drift apart on what an id is
// (ADR-0005: "one id, every surface").
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "mcp://cairn/")
	s = strings.TrimPrefix(s, "run/")
	s = strings.TrimPrefix(s, "hook/")
	return s
}

// gen draws n unbiased base62 characters using rejection sampling over the
// random byte space so every character is uniformly distributed.
func gen(n int) (string, error) {
	const max = 62 * 4 // largest multiple of 62 <= 256; bytes >= max are rejected
	out := make([]byte, n)
	buf := make([]byte, n)
	filled := 0
	for filled < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("id: read random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= max {
				continue // reject to avoid modulo bias
			}
			out[filled] = alphabet[int(b)%62]
			filled++
			if filled == n {
				break
			}
		}
	}
	return string(out), nil
}
