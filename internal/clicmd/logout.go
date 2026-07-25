package clicmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joestump/cairn/internal/cliconfig"
)

// newLogoutCmd builds `cairn logout` (SPEC-0008 "Authentication and Session
// Lifecycle"; cairn#21). It deletes whatever credential `cairn login`
// stored — the OS keyring entry, the 0600 config-file token, or both — for
// the currently-resolved server. Logout is idempotent: running it with no
// active session is not an error (SPEC-0008 logout's revoke-then-forget
// lifecycle should never itself fail the command).
//
// Server-side revocation (RFC 7009, `POST /v1/oauth/revoke`) applies to
// OAuth grants; this issue's token seam (a static CAIRN_API_TOKENS entry or
// a personal access token, ADR-0004) has no per-CLI-session grant for the
// server to revoke — the credential belongs to the human, not this one
// login — so logout's job here is exactly "removes stored credential", per
// cairn#21's task list. Revoking an OAuth grant lands with the browser flow
// (cairn#22 and on).
func newLogoutCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove this machine's stored credential",
		Long: `Deletes the locally stored bearer token — from the OS keyring and/or the
0600 config file, wherever cairn login put it. It does not affect the
token's validity on the server or any other machine's stored copy.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogout(streams, flags, configPathOverride)
		},
	}
}

func runLogout(streams IOStreams, flags *globalFlags, configPathOverride string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	removed, err := cliconfig.DeleteCredential(configPathOverride, cfg.APIBaseURL)
	if err != nil {
		return err
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, map[string]any{"authenticated": false, "removed": removed})
	}
	if removed {
		fmt.Fprintln(streams.Out, "✓ logged out")
	} else {
		fmt.Fprintln(streams.Out, "not logged in — nothing to remove")
	}
	return nil
}
