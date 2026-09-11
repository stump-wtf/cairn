package clicmd

import "strings"

// parseTagFlags collects repeated `--tag` flags, each holding one tag or a
// comma-separated list, in the order given. An empty tag is a usage error,
// reported before any network call; the charset, size and count bounds and
// deduplication are the server's (SPEC-0008 "Data the CLI shows but never
// decides"), so a server that widens them needs no CLI release.
//
// Governing: ADR-0018 (Client-Asserted Artifact Tags), SPEC-0008 REQ "Pipe and
// Path Ingest", REQ "Bundle Creation"
func parseTagFlags(raw []string) ([]string, error) {
	var tags []string
	for _, flag := range raw {
		for _, item := range strings.Split(flag, ",") {
			t := strings.TrimSpace(item)
			if t == "" {
				return nil, usageErrorf("--tag %q contains an empty tag", flag)
			}
			tags = append(tags, t)
		}
	}
	return tags, nil
}
