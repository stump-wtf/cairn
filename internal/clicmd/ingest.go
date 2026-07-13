package clicmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/joestump/cairn/internal/cliclient"
)

// runIngest implements the bare `cairn` command (SPEC-0008 "Pipe and Path
// Ingest"): stdin content or a single path argument becomes one artifact.
// Multiple path arguments are a bundle and belong to `cairn add` — SPEC-0008's
// command table gives bundles their own command, so ambiguity is a usage
// error here rather than silently picking a behavior.
func runIngest(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string, args []string) error {
	if len(args) > 1 {
		return usageErrorf("cairn accepts a single file; use `cairn add` to push multiple files as a bundle")
	}

	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}

	// Determine and validate the body *before* checking auth: an empty
	// stdin or a bad path is a usage error regardless of authentication
	// state, and usage errors are reported "before any network call"
	// (SPEC-0008 "Configuration and Server Endpoint Resolution") — the same
	// principle extends to input validation generally.
	var (
		body      io.Reader
		mediaType string
		title     string
	)

	if len(args) == 1 {
		path := args[0]
		f, err := os.Open(path)
		if err != nil {
			return usageErrorf("open %s: %v", path, err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return usageErrorf("stat %s: %v", path, err)
		}
		if info.IsDir() {
			return usageErrorf("%s is a directory; pass files, not directories", path)
		}
		body = f
		title = filepath.Base(path)
	} else {
		// No path argument: read the piped body. A stdin that is itself an
		// interactive terminal (no pipe, no path) is "empty input" per
		// SPEC-0008 ("Empty input" scenario) — show help instead of hanging
		// waiting for a human to type and Ctrl-D.
		if isTerminal(streams.Out) && isTerminal(streams.In) {
			_ = cmd.Help()
			return usageErrorf("no input: pipe content on stdin or pass a file path")
		}
		br := bufio.NewReader(streams.In)
		// Peek so a genuinely empty stdin (immediate EOF) is rejected before
		// any request is attempted, rather than creating an empty artifact.
		if _, err := br.Peek(1); err == io.EOF {
			return usageErrorf("stdin is empty: pipe content on stdin or pass a file path")
		}
		body = br
	}

	if cfg.Token == "" {
		return fmt.Errorf("not authenticated: run `cairn login` (or set --token/CAIRN_TOKEN): %w", cliclient.ErrNotAuthenticated)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	art, err := client.CreateArtifact(cmd.Context(), body, cliclient.CreateArtifactOptions{
		Title:     title,
		MediaType: mediaType,
	})
	if err != nil {
		return err
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, art)
	}
	fmt.Fprintln(streams.Out, art.URL)
	return nil
}
