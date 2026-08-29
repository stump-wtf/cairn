package clicmd

import (
	"os"

	"github.com/mattn/go-isatty"
)

// IsTerminal reports whether s (an io.Reader or io.Writer backed by an
// *os.File) is an interactive terminal. Piped/redirected streams and the
// in-memory buffers tests substitute are never terminals (SPEC-0008 "TTY,
// Piping, and Clipboard Behavior"). Exported for cmd/cairn/main, which
// decides whether fang's styled error rendering applies.
func IsTerminal(s any) bool {
	f, ok := s.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
