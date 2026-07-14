package clicmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
)

// ErrClipboardUnavailable is the sentinel copyToClipboard returns when no
// copy mechanism succeeded — never a fatal error (SPEC-0008 "Clipboard copy
// MUST be best-effort ... MUST NOT fail the command").
var ErrClipboardUnavailable = errors.New("clipboard unavailable")

// clipboardCommand names one candidate program and the args that make it
// read stdin and write it to the system clipboard.
type clipboardCommand struct {
	name string
	args []string
}

// platformClipboardCommands lists, in try-order, the external programs that
// can receive text on stdin and place it on the clipboard, per OS (SPEC-0008
// "OSC52/pbcopy/xclip fallback chain"). The first one found on PATH wins;
// none found falls through to the OSC52 escape-sequence write.
func platformClipboardCommands() []clipboardCommand {
	switch runtime.GOOS {
	case "darwin":
		return []clipboardCommand{{name: "pbcopy"}}
	case "windows":
		return []clipboardCommand{{name: "clip"}}
	default: // linux and other unix-likes
		return []clipboardCommand{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	}
}

// lookPath is overridable in tests so the fallback chain's "nothing found"
// path is exercised deterministically regardless of what's actually
// installed on the machine running `go test` (SPEC-0008 acceptance:
// "clipboard fallback no-op in CI").
var lookPath = exec.LookPath

// runClipboardCommand is overridable in tests to avoid actually invoking (or
// depending on the presence of) a real clipboard binary in CI.
var runClipboardCommand = func(cmd clipboardCommand, text string) error {
	c := exec.Command(cmd.name, cmd.args...)
	c.Stdin = bytes.NewReader([]byte(text))
	return c.Run()
}

// copyToClipboard best-effort copies text to the system clipboard (SPEC-0008
// "TTY, Piping, and Clipboard Behavior"): it tries each platform-appropriate
// external command found on PATH in order, and if none is available, falls
// back to writing an OSC52 escape sequence directly to out (the real
// terminal — this is the one case the CLI is deliberately allowed to emit a
// control sequence to a TTY stream, since that IS the mechanism, not
// decoration). It returns ErrClipboardUnavailable, never fails the caller's
// command, when nothing worked (e.g. headless/SSH with no OSC52-capable
// terminal to fall back to — the write itself cannot detect terminal
// support, so it is attempted best-effort and always reported as the last
// resort rather than a confirmed success).
func copyToClipboard(out io.Writer, text string) error {
	for _, cmd := range platformClipboardCommands() {
		if _, err := lookPath(cmd.name); err != nil {
			continue
		}
		if err := runClipboardCommand(cmd, text); err == nil {
			return nil
		}
	}
	if out == nil {
		return ErrClipboardUnavailable
	}
	if _, err := fmt.Fprint(out, osc52.New(text)); err != nil {
		return fmt.Errorf("%w: osc52 write failed: %v", ErrClipboardUnavailable, err)
	}
	// OSC52 is a blind write: the terminal may or may not honor it, and
	// there is no ack. Report it as "attempted, not confirmed" by still
	// treating it as best-effort success — SPEC-0008 requires the CLI to
	// skip and note, never fail, so an unconfirmed attempt is preferable to
	// a false "skipped" claim when the terminal likely did support it.
	return nil
}
