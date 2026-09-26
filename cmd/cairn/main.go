// Command cairn is the human's terminal surface for Cairn (SPEC-0008): a
// separate static Go binary that speaks only the documented core /v1
// REST/JSON contract (ADR-0003 "Triple Surface Parity" — the CLI is a pure
// network client, not an in-process adapter, and carries no domain logic).
//
// This entry point owns process concerns only — signal handling and the
// exit-code mapping (SPEC-0008 "Machine-Readable Error Mapping and Exit
// Codes") — and delegates the command tree to internal/clicmd. Help and
// usage-error rendering run through charm.land/fang/v2 (the same surface
// Crush uses), re-skinned with the Cairn design language by
// internal/clicmd.CairnColorScheme.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	fang "charm.land/fang/v2"

	"github.com/stump-wtf/cairn/internal/clicmd"
	"github.com/stump-wtf/cairn/internal/cliexit"
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

	// fang owns the styled rendering of whatever error ExecuteContext
	// returns. Cairn keeps its own stable stderr contract (SPEC-0008
	// "Machine-Readable Error Mapping and Exit Codes"), so the handler
	// routes: usage errors get fang's ERROR layout (Cairn-skinned, on an
	// interactive terminal only), everything else keeps the greppable
	// "cairn: <tag>: <message>" shape or its --json envelope, and a
	// SIGINT-canceled error prints nothing here — main's interrupted
	// branch below owns that message and exit code 130.
	errHandler := func(w io.Writer, styles fang.Styles, err error) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		if errors.Is(err, cliexit.ErrUsage) && clicmd.IsTerminal(streams.ErrOut) {
			// usageErrorf's ErrUsage sentinel is classification, not
			// prose: strip it before display so the styled message reads
			// "unknown flag: --bogus", not "…: cairn: usage error".
			msg := strings.TrimSuffix(err.Error(), ": "+cliexit.ErrUsage.Error())
			fang.DefaultErrorHandler(w, styles, errors.New(msg))
			return
		}
		clicmd.PrintError(streams.ErrOut, err, root)
	}

	err := fang.Execute(
		ctx,
		root,
		fang.WithVersion(clicmd.VersionString()),
		fang.WithColorSchemeFunc(clicmd.CairnColorScheme),
		fang.WithErrorHandler(errHandler),
	)

	// SIGINT during an in-flight request cancels ctx; that shows up as
	// context.Canceled (possibly wrapped) regardless of which layer
	// observed it first, and always means exit 130 (SPEC-0008 "Ctrl-C
	// mid-bundle": "cancel the shared context ... exit 130"), not whatever
	// generic error the canceled request happened to return.
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		clicmd.PrintInterrupted(streams.ErrOut)
		return int(cliexit.Interrupted)
	}

	return int(cliexit.ForError(err))
}
