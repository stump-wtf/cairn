package clicmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// PrintError is cmd/cairn/main's entry point into this package's stable
// error formatting (SPEC-0008 "Machine-Readable Error Mapping and Exit
// Codes"). root is the command returned by NewRootCmd, already executed —
// its --json flag (shared across every subcommand via the persistent flag
// binding) decides human vs. JSON error output.
func PrintError(w io.Writer, err error, root *cobra.Command) {
	jsonOut, _ := root.PersistentFlags().GetBool("json")
	printError(w, err, jsonOut)
}

// PrintInterrupted writes the SIGINT message for exit code 130.
func PrintInterrupted(w io.Writer) {
	fmt.Fprintln(w, "cairn: interrupted")
}
