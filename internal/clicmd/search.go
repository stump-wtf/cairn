// The `cairn search` command tree.
//
// Only the operator half exists today: `cairn search reindex`, which rebuilds
// the lexical index (SPEC-0022 REQ-11). The query half — `cairn search "…"`
// against the REST search endpoint — lands with the follow-up story that ships
// GET /v1/search, and running the command without `reindex` says so instead of
// printing a generic usage error.
//
// reindex is a server-side command: it reads the CAIRN_* environment and works
// on the database directly, exactly like `cairn serve`, rather than making a
// request. A rebuild is not something a remote client should be able to ask for
// (SPEC-0008 "Command Surface and REST-Client Boundary" binds the CLI's other
// commands to the API; this one is an operator tool, like serve).
//
// Governing: ADR-0028, SPEC-0022 REQ-11, ADR-0031 (one binary: this is what the
// issue's `cairnd search reindex` spelling became).
package clicmd

import (
	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/serve"
)

func newSearchCmd(streams IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search artifacts (operator: rebuild the index)",
		Long: `Search Cairn's lexical index.

The query surface is the next story: GET /v1/search and the artifact_search MCP
tool are not shipped yet, so "cairn search <query>" is not available.

What is here is the rebuild every search depends on:

  cairn search reindex                 rebuild every live artifact's document
  cairn search reindex --owner <id>    rebuild one user's or team's documents

reindex is run on the server host, reads the same CAIRN_* environment as
"cairn serve", and does not take an API token: it works on the database
directly. It can run while the server is up.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return usageErrorf("`cairn search <query>` is not shipped yet: the query surface is the next " +
				"SPEC-0022 story; use `cairn search reindex` to rebuild the index")
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newSearchReindexCmd(streams))
	return cmd
}

func newSearchReindexCmd(streams IOStreams) *cobra.Command {
	var owner string
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the lexical search index for live artifacts",
		Long: `Rebuild the lexical search index.

Every live artifact is queued and indexed in this process, so the command exiting
cleanly means the index is current. Expired artifacts are left alone: they are
invisible to search and the reaper is removing them.

Run it after deploying the indexer, after a give-up the server logged, or to
rebuild one workspace's documents with --owner.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve.Reindex(cmd.Context(), owner, streams.Out)
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "",
		"rebuild only this user's or team's artifacts (id); default: every live artifact")
	return cmd
}
