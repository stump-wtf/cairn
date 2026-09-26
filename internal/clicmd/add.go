package clicmd

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/cliexit"
)

// newAddCmd builds `cairn add f1 f2 ...` (SPEC-0008 "Bundle Creation").
// --title/--ttl/--type/--no-copy/--concurrency are bound once at the root
// (clicmd.go) and shared with the bare ingest command.
func newAddCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	return &cobra.Command{
		Use:   "add <file>...",
		Short: "Push several files as one bundle artifact",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdd(cmd, streams, flags, configPathOverride, args)
		},
	}
}

func runAdd(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string, paths []string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	ttlSeconds, err := parseTTLFlag(flags.ttl)
	if err != nil {
		return err
	}
	tags, err := parseTagFlags(flags.tags)
	if err != nil {
		return err
	}

	// Local file validation (missing path, duplicate path, a directory) is a
	// usage error and is checked before auth/network, same as the bare
	// ingest command (SPEC-0008 "Oversize and Duplicate File Handling").
	deduped, err := cliclient.ValidateBundlePaths(paths)
	if err != nil {
		return usageErrorf("%v", err)
	}

	if cfg.Token == "" {
		return fmt.Errorf("not authenticated: run `cairn login` (or set --token/CAIRN_TOKEN): %w", cliclient.ErrNotAuthenticated)
	}

	concurrency := flags.concurrency
	if concurrency < 1 {
		concurrency = defaultUploadConcurrency
	}

	// A child context so Ctrl-C inside the Bubble Tea TUI can cancel
	// in-flight reads/uploads even when raw terminal mode has disabled ISIG
	// and the OS-level SIGINT signal.NotifyContext relies on (cmd/cairn/
	// main.go) never fires (SPEC-0008 "Ctrl-C mid-bundle").
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	opts := cliclient.CreateBundleOptions{Title: flags.title, TTLSeconds: ttlSeconds, Tags: tags}

	var (
		art         *cliclient.Artifact
		interrupted bool
	)
	if IsTerminal(streams.ErrOut) && !flags.jsonOut {
		art, interrupted, err = runAddInteractive(ctx, streams, client, deduped, opts, concurrency, cancel)
	} else {
		var files []cliclient.BundleFile
		files, err = cliclient.PrepareBundleFilesConcurrent(ctx, deduped, concurrency, nil)
		if err == nil {
			art, err = client.CreateBundle(ctx, files, opts)
		}
	}

	if interrupted {
		return cliexit.ErrInterrupted
	}
	if err != nil {
		return err
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, art)
	}

	fmt.Fprintf(streams.Out, "%d files · %s · ⧗ expires %s · 🔒 %s\n",
		len(deduped), formatSize(art.Size), formatTTL(art.ExpiresAt), formatAccess(art.Visibility))
	fmt.Fprintln(streams.Out, art.URL)

	if !flags.noCopy && IsTerminal(streams.Out) {
		if err := copyToClipboard(streams.ErrOut, art.URL); err != nil {
			fmt.Fprintln(streams.ErrOut, "cairn: clipboard unavailable, copy skipped")
		}
	}
	return nil
}

// runAddInteractive drives cairn add's Bubble Tea per-file progress display
// (SPEC-0008 "per-file progress rows ... Lip Gloss + Bubbles progress")
// while the bounded worker pool prepares files concurrently in the
// background and, once every file is ready, sends the single atomic bundle
// POST — mirroring cli/design.md's "Worker pool (bounded, shared ctx)"
// sequence.
func runAddInteractive(ctx context.Context, streams IOStreams, client *cliclient.Client, paths []string, opts cliclient.CreateBundleOptions, concurrency int, cancel context.CancelFunc) (art *cliclient.Artifact, interrupted bool, err error) {
	model := newBundleModel(paths, cancel)
	program := tea.NewProgram(model, tea.WithOutput(streams.ErrOut), tea.WithInput(streams.In), tea.WithContext(ctx))

	go func() {
		files, perr := cliclient.PrepareBundleFilesConcurrent(ctx, paths, concurrency, func(fp cliclient.FileProgress) {
			program.Send(bundleProgressMsg(fp))
		})
		if perr != nil {
			program.Send(bundleResultMsg{err: perr})
			return
		}
		a, cerr := client.CreateBundle(ctx, files, opts)
		program.Send(bundleResultMsg{art: a, err: cerr})
	}()

	finalModel, runErr := program.Run()
	if runErr != nil {
		return nil, false, fmt.Errorf("cairn add: TUI: %w", runErr)
	}
	fm, ok := finalModel.(bundleModel)
	if !ok {
		return nil, false, fmt.Errorf("cairn add: unexpected TUI model type %T", finalModel)
	}
	if fm.interrupt {
		return nil, true, nil
	}
	if fm.result == nil {
		return nil, false, fmt.Errorf("cairn add: TUI exited with no result")
	}
	return fm.result.art, false, fm.result.err
}
