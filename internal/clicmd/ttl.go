package clicmd

import (
	"strconv"
	"strings"
	"time"
)

// parseTTLFlag parses the `--ttl` flag's value into a whole number of
// seconds to send as X-Cairn-Ttl-Seconds. It accepts Go's duration syntax
// ("24h", "90m") extended with a "d" (day) unit Go's time.ParseDuration
// lacks — "7d" is the natural way a human spells a week-ish TTL — and a bare
// integer, interpreted as seconds. This is unit conversion, not policy: the
// CLI never decides whether a TTL is *allowed*, only what number of seconds
// the human's string means; the server is authoritative over whether to
// honor it (SPEC-0008 "Data the CLI shows but never decides").
//
// An empty s returns (0, nil): "no explicit TTL requested," which callers
// treat as "let the server assign its default."
func parseTTLFlag(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Bare integer: seconds.
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		if secs <= 0 {
			return 0, usageErrorf("--ttl must be positive, got %q", s)
		}
		return secs, nil
	}

	// "<n>d" day suffix, since time.ParseDuration has no day unit.
	if strings.HasSuffix(s, "d") {
		numPart := strings.TrimSuffix(s, "d")
		if days, err := strconv.ParseFloat(numPart, 64); err == nil {
			if days <= 0 {
				return 0, usageErrorf("--ttl must be positive, got %q", s)
			}
			return int64(days * 24 * float64(time.Hour) / float64(time.Second)), nil
		}
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, usageErrorf("--ttl %q is not a valid duration (try \"24h\", \"7d\", \"30m\")", s)
	}
	if d <= 0 {
		return 0, usageErrorf("--ttl must be positive, got %q", s)
	}
	return int64(d / time.Second), nil
}

// formatTTLSeconds renders a whole number of seconds in the largest unit that
// divides it exactly — days, then hours, minutes, seconds — in the syntax
// parseTTLFlag reads back, so a limit the server states (2592000 seconds)
// reads as the flag a user would type (30d).
//
// Governing: ADR-0025, SPEC-0019 VE-8
func formatTTLSeconds(secs int64) string {
	units := []struct {
		suffix string
		secs   int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}}
	for _, u := range units {
		if secs != 0 && secs%u.secs == 0 {
			return strconv.FormatInt(secs/u.secs, 10) + u.suffix
		}
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// formatAccess renders the server's visibility value the way SPEC-0008's
// decorated output wants it ("🔒 you + anyone with link"). It is purely
// presentational — the CLI never decides access, only displays what the
// server returned (SPEC-0008 "Data the CLI shows but never decides").
func formatAccess(visibility string) string {
	switch visibility {
	case "link":
		return "you + anyone with link"
	case "private":
		return "only you"
	default:
		if visibility == "" {
			return "unknown"
		}
		return visibility
	}
}
