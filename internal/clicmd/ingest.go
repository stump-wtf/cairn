package clicmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
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

	ttlSeconds, err := parseTTLFlag(flags.ttl)
	if err != nil {
		return err
	}
	tags, err := parseTagFlags(flags.tags, streams.ErrOut)
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
		mediaType = detectMediaType(flags.mediaType, path, nil)
	} else {
		// No path argument: read the piped body. A stdin that is itself an
		// interactive terminal (no pipe, no path) is "empty input" per
		// SPEC-0008 ("Empty input" scenario) — show help instead of hanging
		// waiting for a human to type and Ctrl-D.
		if IsTerminal(streams.Out) && IsTerminal(streams.In) {
			_ = cmd.Help()
			return usageErrorf("no input: pipe content on stdin or pass a file path")
		}
		br := bufio.NewReader(streams.In)
		// Peek so a genuinely empty stdin (immediate EOF) is rejected before
		// any request is attempted, rather than creating an empty artifact,
		// and so the same peeked bytes double as a content-sniff sample for
		// media-type detection (SPEC-0008 "media-type detection (md by
		// content/extension hint flag)") since stdin has no extension to
		// hint from.
		peek, err := br.Peek(512)
		if err != nil && err != io.EOF {
			return fmt.Errorf("read stdin: %w", err)
		}
		if len(peek) == 0 {
			return usageErrorf("stdin is empty: pipe content on stdin or pass a file path")
		}
		body = br
		mediaType = detectMediaType(flags.mediaType, "", peek)
	}
	if flags.title != "" {
		title = flags.title
	}

	if cfg.Token == "" {
		return fmt.Errorf("not authenticated: run `cairn login` (or set --token/CAIRN_TOKEN): %w", cliclient.ErrNotAuthenticated)
	}

	interactive := IsTerminal(streams.Out) && !flags.jsonOut

	// Wrap body in a byte counter for the spinner regardless of whether the
	// spinner actually renders, so the two code paths (with/without a
	// visible spinner) push identical bytes through identical plumbing —
	// only the presence of the animation differs.
	counting := &countingReader{r: body}
	var sp *spinner
	if interactive && IsTerminal(streams.ErrOut) {
		sp = newSpinner(streams.ErrOut, "uploading", counting)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	art, err := client.CreateArtifact(cmd.Context(), counting, cliclient.CreateArtifactOptions{
		Title:      title,
		MediaType:  mediaType,
		TTLSeconds: ttlSeconds,
		Tags:       tags,
	})
	if sp != nil {
		sp.Stop()
	}
	if err != nil {
		return err
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, art)
	}

	if !interactive {
		// Piped/redirected stdout: only the bare link, no decoration, no
		// clipboard copy (SPEC-0008 "Piped into another command").
		fmt.Fprintln(streams.Out, art.URL)
		return nil
	}

	fmt.Fprintln(streams.Out, "✓ pushed")
	fmt.Fprintf(streams.Out, "🔗 %s\n", art.URL)
	fmt.Fprintf(streams.Out, "⧗ expires %s · 🔒 %s\n", formatTTL(art.ExpiresAt), formatAccess(art.Visibility))

	if !flags.noCopy {
		if err := copyToClipboard(streams.ErrOut, art.URL); err != nil {
			fmt.Fprintln(streams.ErrOut, "cairn: clipboard unavailable, copy skipped")
		}
	}
	return nil
}
