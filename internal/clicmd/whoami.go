package clicmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
)

// whoamiResult is the --json payload for `cairn whoami`.
type whoamiResult struct {
	Authenticated bool   `json:"authenticated"`
	ActorID       string `json:"actor_id,omitempty"`
	Channel       string `json:"channel,omitempty"`
	TokenSource   string `json:"token_source,omitempty"`
	APIBaseURL    string `json:"api_base_url"`
}

// newWhoamiCmd builds `cairn whoami` (SPEC-0008 "Authentication and Session
// Lifecycle", cairn#21). Unlike a purely local flag check, whoami
// re-verifies the stored token against the server on every invocation — the
// same GET /v1/whoami round trip `cairn login` uses to validate a candidate
// token — so a token that was revoked or expired server-side is correctly
// reported as not-authenticated even though a (now-stale) value is still on
// disk or in the keyring.
func newWhoamiCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the current authentication state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWhoami(cmd, streams, flags, configPathOverride)
		},
	}
}

func runWhoami(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	if cfg.Token == "" {
		// No-session is a failure (exit 3): route it through the same error
		// path as every other command so --json mode emits the one
		// consistent error envelope to stderr (SPEC-0008 "JSON error
		// payload") instead of a bespoke stdout object — stdout MUST NOT
		// contain anything resembling a success payload on failure. This is
		// purely local: SPEC-0008's "whoami with no session" scenario asks
		// for no stored credentials to fail fast, with no network call.
		if !flags.jsonOut {
			fmt.Fprintln(streams.ErrOut, "cairn: not authenticated — run `cairn login`")
		}
		return fmt.Errorf("not authenticated: %w", cliclient.ErrNotAuthenticated)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	who, err := client.Whoami(cmd.Context())
	if err != nil {
		if errors.Is(err, cliclient.ErrNotAuthenticated) && !flags.jsonOut {
			fmt.Fprintln(streams.ErrOut, "cairn: not authenticated — the stored token was rejected by the server; run `cairn login`")
		}
		return err
	}

	if flags.verbose {
		fmt.Fprintf(streams.ErrOut, "cairn: verbose: whoami token=%s source=%s api_base_url=%s actor=%s\n",
			redactToken(cfg.Token), cfg.TokenSource, cfg.APIBaseURL, who.ActorID)
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, whoamiResult{
			Authenticated: true,
			ActorID:       who.ActorID,
			Channel:       who.Channel,
			TokenSource:   string(cfg.TokenSource),
			APIBaseURL:    cfg.APIBaseURL,
		})
	}
	fmt.Fprintf(streams.Out, "✓ authorized as %s · %s\n", who.ActorID, who.Channel)
	fmt.Fprintf(streams.Out, "token source: %s · server: %s\n", cfg.TokenSource, cfg.APIBaseURL)
	return nil
}
