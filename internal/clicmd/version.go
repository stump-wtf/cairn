package clicmd

// Version, Commit, and Date are overridden at build time via
// -ldflags "-X github.com/joestump/cairn/internal/clicmd.Version=...". They
// back `cairn --version` (SPEC-0008 command skeleton).
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
