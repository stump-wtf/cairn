// Package clicmd builds the `cairn` command tree (SPEC-0008 "Command
// Surface and REST-Client Boundary"): the bare pipe/path ingest command,
// `add`, `login`, `logout`, and `whoami`. Every command funnels through
// internal/cliconfig for configuration resolution and internal/cliclient
// for the REST round trip and error decoding; cmd/cairn/main.go maps the
// returned error to a process exit code via internal/cliexit.
//
// This package carries no domain logic — it treats the server's response as
// authoritative (ADR-0003) — and it is deliberately thin: `login`/`logout`
// are scaffolded here with the command surface, flags, and help text SPEC-0008
// specifies, but the OAuth 2.1 + PKCE flow and secure token storage they
// describe land in cairn#21 (see the stubbed RunE bodies below). `add`
// uploads as a single atomic request rather than the bounded concurrent
// uploader with per-file progress (cairn#22).
//
// Governing: SPEC-0008 (The cairn Command-Line Interface), ADR-0003 (Triple
// Surface Parity), ADR-0012 (Backend Platform and API Shape).
package clicmd

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/joestump/cairn/internal/cliconfig"
)

// IOStreams lets tests substitute stdin/stdout/stderr instead of the
// process's real streams.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

// globalFlags holds the persistent flag destinations shared by every
// subcommand, bound once on the root command.
type globalFlags struct {
	apiURL  string
	token   string
	jsonOut bool
	verbose bool
}

// NewRootCmd builds the full `cairn` command tree. streams lets callers
// (main, tests) control I/O; configOverride, when non-nil, replaces the
// default ~/.config/cairn/config.toml lookup — tests use it to avoid
// touching the real filesystem.
func NewRootCmd(streams IOStreams, configPathOverride string) *cobra.Command {
	flags := &globalFlags{}

	root := &cobra.Command{
		Use:   "cairn [file...]",
		Short: "cairn is pbcopy for cairn.sh: pipe or pass files in, get a shareable link back",
		Long: `cairn pipes or passes content and files in and prints a shareable,
agent-native cairn.sh/<id> link back — printed to stdout and, on an
interactive terminal, copied to the clipboard.

  cat notes.md | cairn        create one artifact from stdin
  cairn report.pdf            create one artifact from a file
  cairn add a.png b.log c.sql push several files as one bundle
  cairn login                 authenticate with a Cairn server
  cairn whoami                show the current authentication state
  cairn logout                revoke this machine's session

cairn carries no domain logic: expiry, access policy, and provenance are
decided by the server and only ever displayed here (SPEC-0008).`,
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIngest(cmd, streams, flags, configPathOverride, args)
		},
	}
	root.SetVersionTemplate("cairn version {{.Version}}\n")
	root.SetOut(streams.Out)
	root.SetErr(streams.ErrOut)
	root.SetIn(streams.In)

	root.PersistentFlags().StringVar(&flags.apiURL, "url", "", "API base URL (env CAIRN_URL, default "+cliconfig.DefaultAPIBaseURL+")")
	root.PersistentFlags().StringVar(&flags.token, "token", "", "bearer token (env CAIRN_TOKEN)")
	root.PersistentFlags().BoolVar(&flags.jsonOut, "json", false, "emit machine-readable JSON instead of human-readable output")
	root.PersistentFlags().BoolVarP(&flags.verbose, "verbose", "v", false, "structured diagnostic output on stderr (tokens redacted)")

	// A flag-parse error (unknown flag, bad value) is a usage error by
	// definition; wrap it so cliexit maps it to exit code 2 and print it
	// through the same formatter as every other error for one consistent,
	// greppable stderr shape.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return usageErrorf("%v", err)
	})

	root.AddCommand(
		newAddCmd(streams, flags, configPathOverride),
		newLoginCmd(streams, flags, configPathOverride),
		newLogoutCmd(streams, flags, configPathOverride),
		newWhoamiCmd(streams, flags, configPathOverride),
	)

	return root
}

func versionString() string {
	if Commit == "none" && Date == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ", " + Date + ")"
}

// resolveConfig applies the standard flag → env → file → default
// precedence (internal/cliconfig) using this invocation's global flags.
func resolveConfig(flags *globalFlags, configPathOverride string) (*cliconfig.Config, error) {
	return cliconfig.Resolve(cliconfig.Options{
		FlagAPIBaseURL: flags.apiURL,
		FlagToken:      flags.token,
		ConfigPath:     configPathOverride,
	})
}
