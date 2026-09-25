package main

// Ingest Redaction Startup
//
// cairnd builds the SPEC-0017 scanner before it serves anything: a bad
// allowlist stops startup (RD-2), and the risky store_unscanned oversize
// policy is announced with a WARN naming the variable (RD-7).
//
// Governing: ADR-0023, SPEC-0017 RD-2, RD-7
//
// @joestump 09/25/2026 - Added for cairn#289.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/cairn/internal/config"
)

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestRedactionScannerDefault(t *testing.T) {
	logger, buf := captureLogger()
	s, err := newRedactionScanner(&config.Config{RedactionOversize: "reject"}, logger)
	if err != nil || s == nil {
		t.Fatalf("newRedactionScanner = %v, %v", s, err)
	}
	if buf.Len() != 0 {
		t.Errorf("default config logged %q, want nothing", buf.String())
	}
}

func TestRedactionScannerWarnsOnStoreUnscanned(t *testing.T) {
	logger, buf := captureLogger()
	if _, err := newRedactionScanner(&config.Config{RedactionOversize: "store_unscanned"}, logger); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "CAIRN_REDACTION_OVERSIZE") {
		t.Errorf("log = %q, want a WARN naming CAIRN_REDACTION_OVERSIZE", out)
	}
}

func TestRedactionScannerBadAllowlistStopsStartup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allowlist.toml")
	if err := os.WriteFile(p, []byte("paths = ['''^docs/''']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newRedactionScanner(&config.Config{RedactionAllowlistFile: p}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "paths") {
		t.Errorf("err = %v, want a startup error naming %s and paths", err, p)
	}
}
