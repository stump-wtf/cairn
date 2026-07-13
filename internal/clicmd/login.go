package clicmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned by the login/logout scaffolds below. It
// intentionally does not wrap any cliexit sentinel, so cliexit.ForError maps
// it to the generic Internal exit code (1) — distinct from a usage error
// (the command itself is valid; its implementation is simply not landed
// yet) and from any server-decided code (no request was ever sent).
var errNotImplemented = errors.New("not yet implemented")

// newLoginCmd builds `cairn login` (SPEC-0008 "Authentication and Session
// Lifecycle"). The command surface, flags, and help text are scaffolded
// here; the OAuth 2.1 authorization-code + PKCE flow against /v1/oauth/*
// (SPEC-0007, ADR-0004) — opening a browser, running the loopback redirect
// listener, exchanging the code, and persisting tokens to the OS secret
// store or a 0600 fallback file — lands in cairn#21.
func newLoginCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with a Cairn server (OAuth 2.1 + PKCE)",
		Long: `Runs the same OAuth 2.1 authorization-code + PKCE flow the MCP agent uses,
via a loopback redirect, and stores the issued tokens securely.

Not yet implemented — see https://github.com/joestump/cairn/issues/21.
In the meantime, set --token or CAIRN_TOKEN to an existing bearer token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(streams.ErrOut, "cairn login: OAuth 2.1 + PKCE is not yet implemented (cairn#21); use --token/CAIRN_TOKEN for now")
			return fmt.Errorf("cairn login: %w", errNotImplemented)
		},
	}
}
