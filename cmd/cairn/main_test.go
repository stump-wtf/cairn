package main

// End-to-end smoke tests for the compiled binary: they exercise main's own
// wiring (argv, real os.Stdin/Stdout/Stderr, process exit code) which
// internal/clicmd's in-process tests intentionally substitute away.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cairn-cli-test-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "cairn")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		panic("build cairn binary: " + err.Error() + "\n" + string(out))
	}

	os.Exit(m.Run())
}

func runBinary(t *testing.T, stdin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "CAIRN_URL=", "CAIRN_TOKEN=")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run cairn binary: %v", err)
	}
	return out.String(), errOut.String(), code
}

func TestBinaryVersion(t *testing.T) {
	stdout, _, code := runBinary(t, "", "--version")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "cairn version") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestBinaryWhoamiNoSessionExit3(t *testing.T) {
	_, stderr, code := runBinary(t, "", "whoami")
	if code != 3 {
		t.Errorf("exit code = %d, want 3, stderr=%q", code, stderr)
	}
}

func TestBinaryEmptyStdinExit2(t *testing.T) {
	_, stderr, code := runBinary(t, "", "--token", "tok")
	if code != 2 {
		t.Errorf("exit code = %d, want 2, stderr=%q", code, stderr)
	}
}

func TestBinaryUnreachableServerExit8(t *testing.T) {
	_, stderr, code := runBinary(t, "hi", "--token", "tok", "--url", "http://127.0.0.1:1")
	if code != 8 {
		t.Errorf("exit code = %d, want 8, stderr=%q", code, stderr)
	}
}

// TestBinaryLoginNoTokenExit2 exercises `cairn login` through the real
// compiled binary (argv, real process exit code) without ever reaching
// internal/cliconfig.SaveCredential — an empty piped stdin and no --token
// is a usage error before any credential store (OS keyring or file) is
// touched, so this is safe to run against whatever real OS secret store
// happens to be reachable on the test host (cairn#21).
func TestBinaryLoginNoTokenExit2(t *testing.T) {
	_, stderr, code := runBinary(t, "", "login", "--url", "https://example.invalid")
	if code != 2 {
		t.Errorf("exit code = %d, want 2, stderr=%q", code, stderr)
	}
}
