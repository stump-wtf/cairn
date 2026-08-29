package clicmd

// Version, Commit, and Date are overridden at build time via
// -ldflags "-X github.com/joestump/cairn/internal/clicmd.Version=...". They
// back `cairn --version` (SPEC-0008 command skeleton).
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// VersionString is the `--version` payload: the bare version outside a
// release build, or version+commit+date when ldflags supplied them. main
// hands it to fang, which owns the root command's Version field.
func VersionString() string {
	if Commit == "none" && Date == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ", " + Date + ")"
}
