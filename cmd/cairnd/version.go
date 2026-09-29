package main

// version, commit and date are overridden at release time by goreleaser's
// ldflags (-X main.version=...). They back `cairnd --version`, which exists so
// an operator, or Harness's native runtime pinning cairnd by digest (cairn#361),
// can confirm which build is installed without starting the server: the
// version check must not need a database, an object store or any config.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString mirrors the CLI's clicmd.VersionString: the bare version
// outside a release build, or version+commit+date when ldflags supplied them.
func versionString() string {
	if commit == "none" && date == "unknown" {
		return version
	}
	return version + " (" + commit + ", " + date + ")"
}

// wantsVersion reports whether the arguments ask for the version and nothing
// else. Exactly one argument, so a future subcommand that takes a --version of
// its own is not swallowed here.
func wantsVersion(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--version", "-version":
		return true
	}
	return false
}
