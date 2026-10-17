package clicmd

import (
	"strings"
)

// parseRelationFlags turns `--reply-to`, `--derived-from` and `--follows`, each
// repeatable or comma-separated like `--tag`, into the `<kind>=<id>` wire
// entries the REST surface reads from X-Cairn-Relation headers, grouped in
// reply_to, derived_from, follows order. The kind comes from the flag, never
// from the user's text. An empty id is a usage error, reported before any
// network call; the id grammar, the count cap and the dedup are the server's
// (SPEC-0024), so a server that widens them needs no CLI release, the same
// division of labor as parseTagFlags.
//
// Governing: SPEC-0024 REQ "Declaring Relations on Create", ADR-0030
func parseRelationFlags(replyTo, derivedFrom, follows []string) ([]string, error) {
	var out []string
	for _, spec := range []struct {
		flag string
		kind string
		raw  []string
	}{
		{"reply-to", "reply_to", replyTo},
		{"derived-from", "derived_from", derivedFrom},
		{"follows", "follows", follows},
	} {
		for _, raw := range spec.raw {
			for _, item := range strings.Split(raw, ",") {
				id := strings.TrimSpace(item)
				if id == "" {
					return nil, usageErrorf("--%s %q contains an empty artifact id", spec.flag, raw)
				}
				out = append(out, spec.kind+"="+id)
			}
		}
	}
	return out, nil
}

// traceparentFlag returns the create's trace context value: the flag when
// given, else the TRACEPARENT environment default baked into the flag's
// default (SPEC-0024 REQ "Trace Context on Create"). The server ignores a
// malformed value, so the CLI forwards whatever it was handed.
func traceparentFlag(flagValue string) string {
	return strings.TrimSpace(flagValue)
}
