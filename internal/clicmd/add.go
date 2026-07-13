package clicmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/joestump/cairn/internal/cliclient"
)

// newAddCmd builds `cairn add f1 f2 ...` (SPEC-0008 "Bundle Creation").
func newAddCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	var title string
	cmd := &cobra.Command{
		Use:   "add <file>...",
		Short: "Push several files as one bundle artifact",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdd(cmd, streams, flags, configPathOverride, args, title)
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "optional bundle title")
	return cmd
}

func runAdd(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string, paths []string, title string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	// Local file validation (missing path, duplicate path, a directory) is a
	// usage error and is checked before auth/network, same as the bare
	// ingest command.
	files, closeAll, err := cliclient.OpenBundleFiles(paths)
	if err != nil {
		return usageErrorf("%v", err)
	}
	defer closeAll()

	if cfg.Token == "" {
		return fmt.Errorf("not authenticated: run `cairn login` (or set --token/CAIRN_TOKEN): %w", cliclient.ErrNotAuthenticated)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	art, err := client.CreateBundle(cmd.Context(), files, title)
	if err != nil {
		return err
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, art)
	}
	fmt.Fprintf(streams.Out, "%d files · %s · ⧗ expires %s · 🔒 %s\n",
		len(files), formatSize(art.Size), formatTTL(art.ExpiresAt), art.Visibility)
	fmt.Fprintln(streams.Out, art.URL)
	return nil
}
