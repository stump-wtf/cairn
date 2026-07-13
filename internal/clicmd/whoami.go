package clicmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joestump/cairn/internal/cliclient"
)

// whoamiResult is the --json payload for `cairn whoami`.
type whoamiResult struct {
	Authenticated bool   `json:"authenticated"`
	TokenSource   string `json:"token_source,omitempty"`
	APIBaseURL    string `json:"api_base_url"`
}

// newWhoamiCmd builds `cairn whoami` (SPEC-0008 "Authentication and Session
// Lifecycle"). Full identity verification against the server (token
// introspection / GET /v1/workspaces/{id}) requires the real OAuth tokens
// `cairn login` issues in cairn#21; today whoami reports, entirely locally,
// whether a bearer token is configured and where it came from — which is
// enough to satisfy SPEC-0008's "whoami with no session" scenario without
// any network call.
func newWhoamiCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the current authentication state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWhoami(streams, flags, configPathOverride)
		},
	}
}

func runWhoami(streams IOStreams, flags *globalFlags, configPathOverride string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	if cfg.Token == "" {
		// No-session is a failure (exit 3): route it through the same error
		// path as every other command so --json mode emits the one
		// consistent error envelope to stderr (SPEC-0008 "JSON error
		// payload") instead of a bespoke stdout object — stdout MUST NOT
		// contain anything resembling a success payload on failure.
		if !flags.jsonOut {
			fmt.Fprintln(streams.ErrOut, "cairn: not authenticated — run `cairn login`")
		}
		return fmt.Errorf("not authenticated: %w", cliclient.ErrNotAuthenticated)
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, whoamiResult{
			Authenticated: true,
			TokenSource:   string(cfg.TokenSource),
			APIBaseURL:    cfg.APIBaseURL,
		})
	}
	fmt.Fprintf(streams.Out, "authenticated: token configured (source: %s, server: %s)\n", cfg.TokenSource, cfg.APIBaseURL)
	fmt.Fprintln(streams.Out, "identity verification against the server lands with `cairn login` (cairn#21)")
	return nil
}
