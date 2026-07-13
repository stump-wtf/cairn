package clicmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newLogoutCmd builds `cairn logout` (SPEC-0008 "Authentication and Session
// Lifecycle"). Revoking the current grant (RFC 7009, against
// /v1/oauth/revoke) and deleting locally stored credentials lands with
// `cairn login` in cairn#21 — there is no local token store to clear until
// login can create one.
func newLogoutCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke this machine's session",
		Long: `Revokes the current OAuth grant and deletes locally stored credentials,
leaving other grants (the agent, other machines) untouched.

Not yet implemented — see https://github.com/joestump/cairn/issues/21.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(streams.ErrOut, "cairn logout: not yet implemented (cairn#21)")
			return fmt.Errorf("cairn logout: %w", errNotImplemented)
		},
	}
}
