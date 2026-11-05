package clicmd

import (
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/stump-wtf/cairn/internal/cliclient"
	"github.com/stump-wtf/cairn/internal/id"
)

// newCommentsCmd builds `cairn comments <id>`: the read half of the annotation
// layer, which `cairn comment` only ever wrote to. The web viewer shows a
// comment thread and the REST surface lists one (SPEC-0006 "Threaded Comments"),
// so a CLI with no way to read comments was the odd surface out (ADR-0003,
// ADR-0006), and worse than merely missing. A script that reached for
// `cairn read --json | jq '.comments[]'` got an empty answer instead of an
// error: the read envelope carries comment_count but never the comments, so a
// thread that exists looked exactly like one that does not.
//
// The JSON form repeats the endpoint's own key, one object with a `comments`
// array emitted even when it is empty, so "no comments" is an answer and never
// silence.
//
// Governing: SPEC-0008 "Command Surface and REST-Client Boundary", SPEC-0006
// "Threaded Comments", ADR-0003, ADR-0006, ADR-0007 (link-capability read).
func newCommentsCmd(streams IOStreams, flags *globalFlags, configPathOverride string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comments <id|handle>",
		Short: "List an artifact's comments",
		Long: `Comments lists what people and agents said on an artifact, in thread
order: each root comment followed by its replies, oldest first. Deleted comments
stay in the list as tombstones, so the thread keeps its shape.

Accepts a bare id or an mcp://cairn/<id> handle, and needs no login: like a read
it rides the artifact's link capability.

  cairn comments 9qz1a
  cairn comments mcp://cairn/9qz1a
  cairn comments 9qz1a --json`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("cairn comments takes exactly one artifact id, got %d arguments", len(args))
			}
			return runComments(cmd, streams, flags, configPathOverride, args[0])
		},
	}
	return cmd
}

// commentsResult is `cairn comments --json`'s stdout object. Its one key is the
// endpoint's own, so a jq expression written against the REST shape works
// unchanged. There is no count field because the array is the count, and the CLI
// invents no figures of its own (SPEC-0008 "Data the CLI shows but never
// decides").
type commentsResult struct {
	Comments []cliclient.Comment `json:"comments"`
}

func runComments(cmd *cobra.Command, streams IOStreams, flags *globalFlags, configPathOverride, rawID string) error {
	cfg, err := resolveConfig(flags, configPathOverride)
	if err != nil {
		return err
	}
	publicID := id.Normalize(rawID)
	if publicID == "" {
		return usageErrorf("no artifact id in %q", rawID)
	}

	client := cliclient.New(cfg.APIBaseURL, cfg.Token)
	comments, err := client.ListComments(cmd.Context(), publicID)
	if err != nil {
		return err
	}

	if flags.verbose {
		fmt.Fprintf(streams.ErrOut, "cairn: verbose: comments artifact=%s count=%d source=%s\n",
			publicID, len(comments), cfg.TokenSource)
	}

	if flags.jsonOut {
		return writeJSON(streams.Out, commentsResult{Comments: comments})
	}

	fmt.Fprintf(streams.Out, "%d %s\n", len(comments), commentCountLabel(len(comments)))
	for _, comment := range comments {
		fmt.Fprintln(streams.Out, commentLine(comment))
		fmt.Fprint(streams.Out, indentBody(comment.Body))
	}
	return nil
}

// commentCountLabel keeps the header row's noun honest for a one-comment
// artifact.
func commentCountLabel(count int) string {
	if count == 1 {
		return "comment"
	}
	return "comments"
}

// commentLine is one comment's header row: who said it and when, plus where it
// sits in the thread. Actor identity is the server's to render (it substitutes
// display handles for readers who cannot see the real account), and the CLI
// prints what came back (SPEC-0016 EV-4).
func commentLine(comment cliclient.Comment) string {
	line := fmt.Sprintf("#%d · %s · %s", comment.ID, comment.ActorID, humanize.Time(comment.CreatedAt))
	if comment.ParentID != nil {
		line += fmt.Sprintf(" · ↳ reply to #%d", *comment.ParentID)
	}
	if comment.Deleted {
		line += " · deleted"
	}
	return line
}

// indentBody hangs a comment's body under its header row. A tombstone's body is
// already empty, so a deleted comment prints its header alone.
func indentBody(body string) string {
	trimmed := strings.TrimRight(body, "\n")
	if trimmed == "" {
		return ""
	}
	var out strings.Builder
	for _, line := range strings.Split(trimmed, "\n") {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
