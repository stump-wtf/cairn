// Command cairn is the human's terminal surface for Cairn (SPEC-0008): a
// separate static Go binary that speaks only the documented core /v1
// REST/JSON contract (ADR-0003 "Triple Surface Parity" — the CLI is a pure
// network client, not an in-process adapter, and carries no domain logic).
//
// This entry point owns process concerns only — signal handling and the
// exit-code mapping (SPEC-0008 "Machine-Readable Error Mapping and Exit
// Codes") — and delegates the command tree to internal/clicmd.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/joestump/cairn/internal/clicmd"
	"github.com/joestump/cairn/internal/cliexit"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	streams := clicmd.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}
	root := clicmd.NewRootCmd(streams, "")
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)

	// SIGINT during an in-flight request cancels ctx; that shows up as
	// context.Canceled (possibly wrapped) regardless of which layer
	// observed it first, and always means exit 130 (SPEC-0008 "Ctrl-C
	// mid-bundle": "cancel the shared context ... exit 130"), not whatever
	// generic error the canceled request happened to return.
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		clicmd.PrintInterrupted(streams.ErrOut)
		return int(cliexit.Interrupted)
	}

	if err != nil {
		clicmd.PrintError(streams.ErrOut, err, root)
	}
	return int(cliexit.ForError(err))
}
