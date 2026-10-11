package clicmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/id"
)

// newCommentCmd builds `cairn comment <id> [body]` (SPEC-0008 "Annotation
// Write"). The body is the one argument, `--file`, or stdin — the three ways
// an agent and a human both type it without a quoting hazard. The comment is
// always anchored to the whole artifact; the server validates that anchor
// against the share type (SPEC-0006 "Registry-Gated Anchor Capabilities").
func newCommentCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	var (
		file     string
		parentID int64
	)

	cmd := &cobra.Command{
		Use:   "comment <id|handle> [body]",
		Short: "Comment on an artifact",
		Long: `Comment posts one comment on an artifact and prints its id and the
artifact's link. The body is the argument, a file, or stdin:

  cairn comment 9qz1a "looks good, one nit below"
  cairn comment mcp://cairn/9qz1a --file reply.md
  cat reply.md | cairn comment 9qz1a
  cairn comment 9qz1a --parent 41 "replying to that"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("cairn comment takes an artifact id and at most one body, got %d arguments", len(args))
			}
			parentSet := cmd.Flags().Changed("parent")
			return runComment(cmd, streams, flags, configPathOverride, args, file, parentID, parentSet)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "read the comment body from this file instead of the argument or stdin")
	cmd.Flags().Int64Var(&parentID, "parent", 0, "id of the comment to reply to; a reply inherits its root's anchor")
	return cmd
}

// commentResult is `cairn comment --json`'s stdout object: what landed and
// where it can be read.
type commentResult struct {
	ID         int64  `json:"id"`
	ArtifactID string `json:"artifact_id"`
	URL        string `json:"url"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	ActorID    string `json:"actor_id,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

func runComment(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride string, args []string, file string, parentID int64, parentSet bool) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}
	if file != "" && len(args) == 2 {
		return usageErrorf("--file and a body argument are mutually exclusive")
	}
	publicID := id.Normalize(args[0])
	if publicID == "" {
		return usageErrorf("no artifact id in %q", args[0])
	}
	if parentSet && parentID <= 0 {
		return usageErrorf("--parent must be a positive comment id, got %d", parentID)
	}

	// Auth is checked before any network call: a comment is a write, and
	// SPEC-0008 requires every mutating command to carry a credential rather
	// than send a request the server would reject.
	if cfg.Token == "" {
		return fmt.Errorf("not authenticated: run `cairn login` (or set --token/CAIRN_TOKEN): %w", cliclient.ErrNotAuthenticated)
	}

	body, err := commentBody(streams, args[1:], file)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return usageErrorf("empty comment body")
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)

	// The metadata read comes first, for two reasons: an unknown or expired id
	// fails with exit 5 before stdin is consumed, and the link the command
	// prints afterwards is the server's, never one the CLI reconstructed.
	art, err := client.GetArtifact(cmd.Context(), publicID)
	if err != nil {
		return err
	}

	var parent *int64
	if parentSet {
		parent = &parentID
	}
	comment, err := client.Comment(cmd.Context(), publicID, cliclient.CommentOptions{Body: body, ParentID: parent})
	if err != nil {
		return err
	}

	if flags.verbose {
		fmt.Fprintf(streams.ErrOut, "cairn: verbose: comment id=%d artifact=%s parent=%v\n", comment.ID, art.ID, parent)
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, commentResult{
			ID:         comment.ID,
			ArtifactID: art.ID,
			URL:        art.URL,
			ParentID:   comment.ParentID,
			ActorID:    comment.ActorID,
			CreatedAt:  comment.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	fmt.Fprintf(streams.Out, "✓ commented · #%d\n%s\n", comment.ID, art.URL)
	return nil
}

// commentBody resolves the body from, in order: --file, the argument (unless
// it is the "-" stdin marker), then stdin when it is piped. Nothing typed at a
// terminal counts as a body, so an accidental bare `cairn comment <id>` is a
// usage error rather than a hung read.
func commentBody(streams IOStreams, args []string, file string) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", usageErrorf("read --file %s: %v", file, err)
		}
		return string(raw), nil
	}
	if len(args) == 1 && args[0] != "-" {
		return args[0], nil
	}
	if len(args) == 1 || !IsTerminal(streams.In) {
		return readAll(streams.In)
	}
	return "", usageErrorf("no comment body: pass it as an argument, with --file, or on stdin")
}

func readAll(r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("cairn comment: read body: %w", err)
	}
	return string(raw), nil
}
