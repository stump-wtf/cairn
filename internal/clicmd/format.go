package clicmd

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
)

// formatSize renders a byte count the way SPEC-0008's bundle summary line
// wants it ("N files · <total size> · ...").
func formatSize(n int64) string {
	return humanize.Bytes(uint64(n))
}

// formatTTL renders the time remaining until expiresAt as a short duration
// ("7d", "3h", "45m"), the coarsest unit that still reads at a glance. A
// zero/past expiresAt (not yet set, or already expired) renders as "—".
func formatTTL(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return "—"
	}
	d := time.Until(expiresAt)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
