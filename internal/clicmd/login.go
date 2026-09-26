package clicmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliconfig"
)

// loginResult is the --json payload for `cairn login`.
type loginResult struct {
	Authenticated bool   `json:"authenticated"`
	ActorID       string `json:"actor_id"`
	Channel       string `json:"channel"`
	APIBaseURL    string `json:"api_base_url"`
	TokenSource   string `json:"token_source"`
}

// newLoginCmd builds `cairn login` (SPEC-0008 "Authentication and Session
// Lifecycle"; cairn#21, a scoped takeover of upstream cairn#37 against the
// real ADR-0004 token auth seam — the browser OAuth 2.1 authorization-code +
// PKCE flow SPEC-0008/SPEC-0007 describe follows in a later issue, #22 and
// on). It accepts an already-minted bearer credential — a personal access
// token from the web Settings page (issue #74) or a CAIRN_API_TOKENS entry
// a deployment operator issued — verifies it against the server with a
// whoami round trip, and stores it securely for future commands.
func newLoginCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with a Cairn server using a bearer token",
		Long: `Verifies a bearer token against the server (a whoami round trip) and stores
it securely for future commands: the OS keyring when one is reachable
(macOS Keychain, the Secret Service/libsecret on Linux), otherwise a 0600
file under the config directory.

Mint a personal access token from the Cairn web Settings page, or use a
token your deployment operator issued, then run one of:

  cairn login --token <token>     pass it directly (never echoed, never logged)
  echo "$TOKEN" | cairn login     pipe it in, e.g. from a secrets manager
  cairn login                     interactive prompt with input hidden

This is the interim token-based login against the ADR-0004 auth seam; the
OAuth 2.1 authorization-code + PKCE browser flow the MCP agent uses
(SPEC-0007) is a follow-up (cairn#22 and on).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd, streams, flags, configPathOverride)
		},
	}
}

func runLogin(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string) error {
	// Resolve the API base URL the normal flag → env → file → default way,
	// but deliberately ignore whatever token is currently on file/keyring —
	// login always verifies a FRESH candidate, never the currently-stored
	// one, so re-running `cairn login` after a bad or revoked token still
	// works instead of re-checking the very credential the user is trying
	// to replace.
	cfg, err := cliconfig.Resolve(cliconfig.Options{
		FlagAPIBaseURL: flags.apiURL,
		ConfigPath:     configPathOverride,
	})
	if err != nil {
		return err
	}

	token, err := readToken(streams, flags.token)
	if err != nil {
		return err
	}

	client := cliclient.New(cfg.APIBaseURL, token)
	who, err := client.Whoami(cmd.Context())
	if err != nil {
		return fmt.Errorf("cairn login: %w", err)
	}

	source, err := cliconfig.SaveCredential(configPathOverride, cfg.APIBaseURL, token)
	if err != nil {
		return err
	}

	if flags.verbose {
		fmt.Fprintf(streams.ErrOut, "cairn: verbose: login token=%s source=%s api_base_url=%s actor=%s\n",
			redactToken(token), source, cfg.APIBaseURL, who.ActorID)
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, loginResult{
			Authenticated: true,
			ActorID:       who.ActorID,
			Channel:       who.Channel,
			APIBaseURL:    cfg.APIBaseURL,
			TokenSource:   string(source),
		})
	}
	fmt.Fprintf(streams.Out, "✓ authorized as %s · %s\n", who.ActorID, who.Channel)
	return nil
}
