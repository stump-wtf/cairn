package clicmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/id"
)

// maxInlineReadBodyBytes bounds the body `cairn read --json` inlines. The
// plain (non-JSON) form streams the whole body straight to stdout and is never
// truncated; this ceiling exists only because a JSON body is read by something
// that holds it in memory — a script, or an agent's context window — and a
// silent 200 MB string is worse than a flagged prefix plus the URL to fetch
// the rest from. It is the same ceiling the MCP read tool applies, so a caller
// moving between the two surfaces sees the same truncation semantics. It is a
// var only so a test can walk the truncation path without allocating a MiB.
var maxInlineReadBodyBytes = 1 << 20 // 1 MiB

// readEnvelope is `cairn read --json`'s single stdout object (SPEC-0008 "Output
// Formats and --json Mode"): the artifact's server-decided metadata, a bundle's
// members when no member was addressed, and the inlined body otherwise.
type readEnvelope struct {
	cliclient.ArtifactRead
	// Body is the artifact's (or addressed member's) content, inlined up to
	// maxInlineReadBodyBytes.
	Body string `json:"body,omitempty"`
	// BodyEncoding is "utf8" or "base64", set whenever Body is populated.
	BodyEncoding string `json:"body_encoding,omitempty"`
	// BodyTruncated reports that Body holds only a prefix; the full bytes stay
	// fetchable at URL.
	BodyTruncated bool `json:"body_truncated,omitempty"`
}

// newReadCmd builds `cairn read <id>` (SPEC-0008 "Artifact Read"). The one
// argument is a bare public id or an mcp://cairn/<id> agent handle, because
// that handle is what a handoff work order carries.
func newReadCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	var memberPath string

	cmd := &cobra.Command{
		Use:   "read <id|handle>",
		Short: "Print an artifact's body, or a bundle's file list",
		Long: `Read prints an artifact's body to stdout, or, for a bundle with no
--path, its file list. A bundle member is addressed with --path.

Accepts a bare id or an mcp://cairn/<id> handle:

  cairn read 9qz1a
  cairn read mcp://cairn/9qz1a --path notes.md
  cairn read 9qz1a --json`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("cairn read takes exactly one artifact id, got %d arguments", len(args))
			}
			return runRead(cmd, streams, flags, configPathOverride, args[0], memberPath)
		},
	}
	cmd.Flags().StringVar(&memberPath, "path", "", "name of one bundle member to print instead of the member list")
	return cmd
}

func runRead(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride, rawID, memberPath string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}
	publicID := id.Normalize(rawID)
	if publicID == "" {
		return usageErrorf("no artifact id in %q", rawID)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	art, err := client.GetArtifact(cmd.Context(), publicID)
	if err != nil {
		return err
	}

	if flags.verbose {
		fmt.Fprintf(streams.ErrOut, "cairn: verbose: read share_type=%s size=%d source=%s\n",
			art.ShareType, art.Size, cfg.TokenSource)
	}

	// A bundle has no body of its own: without a member named there is
	// nothing to print but the members, which is also what tells the reader
	// what --path can take (SPEC-0008 "Artifact Read").
	if art.ShareType == "bundle" && memberPath == "" {
		if flags.jsonOut {
			return writeJSON(streams.Out, readEnvelope{ArtifactRead: *art})
		}
		return printMemberList(streams, art)
	}

	body, meta, err := openBodyOrMember(cmd, client, publicID, memberPath)
	if err != nil {
		return err
	}
	defer body.Close()

	// A member's size and checksum are the member's own, not the bundle
	// artifact's (which carries no single body), so the body response's
	// figures replace the metadata's.
	if memberPath != "" {
		if meta.Size > 0 {
			art.Size = meta.Size
		}
		if meta.SHA256 != "" {
			art.Checksum = meta.SHA256
		}
	}

	if flags.jsonOut {
		env, err := inlineBody(readEnvelope{ArtifactRead: *art}, body)
		if err != nil {
			return err
		}
		return writeJSON(streams.Out, env)
	}

	// Piped stdout stays the bare body so `cairn read x | pbcopy` composes;
	// only an interactive read gets the one-line context on stderr.
	if IsTerminal(streams.Out) {
		fmt.Fprintf(streams.ErrOut, "cairn: %s · ⧗ expires %s · 🔒 %s\n",
			readLabel(art), formatTTL(art.ExpiresAt), formatAccess(art.Visibility))
	}
	if _, err := io.Copy(streams.Out, body); err != nil {
		return fmt.Errorf("cairn read: write body: %w", err)
	}
	return nil
}

func openBodyOrMember(cmd *cobra.Command, client *cliclient.Client, publicID, memberPath string) (io.ReadCloser, cliclient.BodyMeta, error) {
	if memberPath == "" {
		return client.OpenBody(cmd.Context(), publicID)
	}
	return client.OpenMember(cmd.Context(), publicID, memberPath)
}

// inlineBody fills env with up to maxInlineReadBodyBytes of body. Reading one
// byte past the cap is what distinguishes "the body was exactly the cap" from
// "there is more at the URL", so no size claim from the metadata is needed.
func inlineBody(env readEnvelope, body io.Reader) (readEnvelope, error) {
	buf := make([]byte, maxInlineReadBodyBytes+1)
	n, err := io.ReadFull(body, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return env, fmt.Errorf("cairn read: read body: %w", err)
	}
	truncated := n > maxInlineReadBodyBytes
	if truncated {
		n = maxInlineReadBodyBytes
	}
	content := buf[:n]
	env.BodyTruncated = truncated
	if utf8.Valid(content) {
		env.Body = string(content)
		env.BodyEncoding = "utf8"
		return env, nil
	}
	env.Body = base64.StdEncoding.EncodeToString(content)
	env.BodyEncoding = "base64"
	return env, nil
}

// printMemberList renders a bundle's members and its link. The counts are the
// server's; the CLI sums nothing (SPEC-0008 "Data the CLI shows but never
// decides").
func printMemberList(streams IOStreams, art *cliclient.ArtifactRead) error {
	var total int64
	for _, m := range art.Members {
		total += m.Size
	}
	fmt.Fprintf(streams.Out, "%d files · %s · ⧗ expires %s · 🔒 %s\n",
		len(art.Members), formatSize(total), formatTTL(art.ExpiresAt), formatAccess(art.Visibility))
	for _, m := range art.Members {
		fmt.Fprintf(streams.Out, "  %-40s %8s  %s\n", m.Name, formatSize(m.Size), m.MediaType)
	}
	fmt.Fprintln(streams.Out, art.URL)
	return nil
}

// readLabel is the human one-liner's leading token: the title when the
// artifact has one, otherwise its id.
func readLabel(art *cliclient.ArtifactRead) string {
	if strings.TrimSpace(art.Title) != "" {
		return art.Title
	}
	return art.ID
}
