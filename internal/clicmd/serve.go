// The `cairn serve` subcommand: the Cairn server, in the one binary (ADR-0031).
//
// Governing: ADR-0031 (One Cairn Binary — The Server Is `cairn serve`),
// ADR-0012 (Backend Platform), SPEC-0008 (Machine-Readable Error Mapping and
// Exit Codes). The configuration contract is the former cairnd entry point's,
// unchanged: the server is configured entirely through CAIRN_* environment
// variables and takes no flags beyond --help/--version.
package clicmd

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/serve"
)

func newServeCmd(streams IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Cairn server (web app, /v1 API, SSE, MCP) in this process",
		Long: `Run the Cairn server in this process.

serve starts the web app, the /v1 REST/JSON API, the SSE streams and the MCP
server, wired over one core service (ADR-0012). It is the former cairnd
binary, folded into cairn as a subcommand (ADR-0031).

Configuration comes entirely from the CAIRN_* environment — the same
variables, with the same meanings, cairnd read: CAIRN_HTTP_ADDR,
CAIRN_DATABASE_URL, CAIRN_S3_ENDPOINT and friends. There are no flags; a
deployment's environment is the whole contract. SIGINT/SIGTERM shut the
server down gracefully.`,
		Version:       VersionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(streams.Out, nil))
			return serve.Run(cmd.Context(), logger)
		},
	}
	cmd.SetVersionTemplate("cairn version {{.Version}}\n")
	return cmd
}
