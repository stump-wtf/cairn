// Command cairnd is a deprecation shim: it execs `cairn serve "$@"` with the
// same arguments and environment (ADR-0031). It exists so the container image
// keeps its `/usr/local/bin/cairnd` ENTRYPOINT through the release that folds
// the two binaries into one — the StumpCloud edge stack runs the image with no
// command override, so the entrypoint is the deploy contract. It will be
// removed in a later release; new deployments invoke `cairn serve` directly.
//
// Exec, not fork: the shim replaces itself, so `cairn serve` remains the
// process the container supervises and keeps its signal handling.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	bin, err := exec.LookPath("cairn")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairnd: cairn binary not found on PATH; install the cairn release (ADR-0031)")
		os.Exit(127)
	}
	args := append([]string{"cairn", "serve"}, os.Args[1:]...)
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "cairnd: exec %s: %v\n", bin, err)
		os.Exit(1)
	}
}
