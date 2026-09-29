package clicmd

import (
	"fmt"
	"io"
	"strings"
)

// parseTagFlags collects repeated `--tag` flags, each holding one tag or a
// comma-separated list, in the order given. An empty tag is a usage error,
// reported before any network call; the charset, size and count bounds and
// deduplication are the server's (SPEC-0008 "Data the CLI shows but never
// decides"), so a server that widens them needs no CLI release.
//
// The one thing the CLI does decide is case: a tag holding ASCII uppercase is
// lower-cased before it is sent, with a warning on errOut, since the server
// rejects uppercase (ADR-0018) and a human typing `size:M` means `size:m`.
// Only A-Z change; every other character, and every other surface, is left to
// the server.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0008 REQ "Pipe and
// Path Ingest", REQ "Bundle Creation", ADR-0025, SPEC-0019 VE-9
func parseTagFlags(raw []string, errOut io.Writer) ([]string, error) {
	var tags []string
	for _, flag := range raw {
		for _, item := range strings.Split(flag, ",") {
			t := strings.TrimSpace(item)
			if t == "" {
				return nil, usageErrorf("--tag %q contains an empty tag", flag)
			}
			if folded := foldASCIIUpper(t); folded != t {
				fmt.Fprintf(errOut, "cairn: warning: tag %q sent as %q\n", t, folded)
				t = folded
			}
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// foldASCIIUpper lower-cases A-Z and nothing else. strings.ToLower would also
// fold non-ASCII letters, which the server rejects as a charset problem, not a
// case one, and which the CLI must pass through unchanged (SPEC-0019 VE-9).
// It works on bytes, so even invalid UTF-8 reaches the server byte for byte.
func foldASCIIUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
