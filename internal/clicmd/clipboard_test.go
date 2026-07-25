package clicmd

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// withMockedClipboardTools replaces lookPath/runClipboardCommand for the
// duration of the test so the fallback chain is exercised deterministically
// without depending on (or actually invoking) whatever clipboard binaries
// happen to be installed on the machine running `go test` — SPEC-0008
// acceptance: "clipboard fallback no-op in CI."
func withMockedClipboardTools(t *testing.T, found map[string]bool, run func(cmd clipboardCommand, text string) error) {
	t.Helper()
	origLookPath, origRun := lookPath, runClipboardCommand
	lookPath = func(name string) (string, error) {
		if found[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	if run != nil {
		runClipboardCommand = run
	}
	t.Cleanup(func() { lookPath, runClipboardCommand = origLookPath, origRun })
}

func TestCopyToClipboardUsesFirstAvailableTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the linux xclip/xsel/wl-copy candidate list")
	}
	var invoked clipboardCommand
	var gotText string
	withMockedClipboardTools(t, map[string]bool{"xclip": true}, func(cmd clipboardCommand, text string) error {
		invoked = cmd
		gotText = text
		return nil
	})

	var out bytes.Buffer
	if err := copyToClipboard(&out, "https://cairn.sh/abc12"); err != nil {
		t.Fatalf("copyToClipboard: %v", err)
	}
	if invoked.name != "xclip" {
		t.Errorf("invoked tool = %q, want xclip", invoked.name)
	}
	if gotText != "https://cairn.sh/abc12" {
		t.Errorf("copied text = %q", gotText)
	}
	// The external tool succeeded, so no OSC52 escape sequence should have
	// been written to out as well.
	if out.Len() != 0 {
		t.Errorf("out = %q, want empty (external tool handled the copy)", out.String())
	}
}

func TestCopyToClipboardFallsBackToOSC52WhenNoToolFound(t *testing.T) {
	withMockedClipboardTools(t, nil, nil) // no tool found on PATH

	var out bytes.Buffer
	if err := copyToClipboard(&out, "https://cairn.sh/abc12"); err != nil {
		t.Fatalf("copyToClipboard: %v (want best-effort success via OSC52)", err)
	}
	if !strings.Contains(out.String(), "\x1b]52") {
		t.Errorf("out = %q, want an OSC52 escape sequence", out.String())
	}
}

func TestCopyToClipboardNoOpWithoutFallbackTarget(t *testing.T) {
	withMockedClipboardTools(t, nil, nil)

	err := copyToClipboard(nil, "https://cairn.sh/abc12")
	if !errors.Is(err, ErrClipboardUnavailable) {
		t.Errorf("err = %v, want ErrClipboardUnavailable", err)
	}
}

func TestCopyToClipboardTriesNextToolOnFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the linux xclip/xsel/wl-copy candidate list")
	}
	var attempts []string
	withMockedClipboardTools(t, map[string]bool{"wl-copy": true, "xclip": true}, func(cmd clipboardCommand, text string) error {
		attempts = append(attempts, cmd.name)
		if cmd.name == "wl-copy" {
			return errors.New("no wayland display")
		}
		return nil
	})

	var out bytes.Buffer
	if err := copyToClipboard(&out, "x"); err != nil {
		t.Fatalf("copyToClipboard: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != "wl-copy" || attempts[1] != "xclip" {
		t.Errorf("attempts = %v, want [wl-copy xclip]", attempts)
	}
}
