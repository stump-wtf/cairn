package cliconfig

import (
	"path/filepath"
	"testing"
)

// TestMain clears CAIRN_URL and CAIRN_TOKEN for the whole package test
// binary (see credential_test.go). The resolver in config.go prefers the
// environment over the config file, so on any box that exports a real token
// (a developer shell, an agent run) every test that expects the default or
// the file value went red, and each failure printed the resolved token into
// the test log — a live PAT in output (#496). This is the package-wide
// equivalent of why internal/clicmd's runCLI helper isolates both variables.

// TestAmbientEnvironmentIsIgnored proves that isolation: it reads the
// environment exactly as an unisolated test would, so on a box that exports
// CAIRN_URL/CAIRN_TOKEN the suite stays green only while TestMain keeps
// clearing them. Resolution must land on the default URL with no token —
// never on whatever credential the invoking shell happens to export, which
// is what used to print a live PAT into the test log. Tests that want the
// env layer (e.g. the precedence test) still set their own dummy values with
// t.Setenv; those are dummies, not the ambient credential.
func TestAmbientEnvironmentIsIgnored(t *testing.T) {
	cfg, err := Resolve(Options{ConfigPath: filepath.Join(t.TempDir(), "missing.toml")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.APIBaseURL != DefaultAPIBaseURL || cfg.URLSource != SourceDefault {
		t.Errorf("URL/Source = %q/%q, want default %q/%q", cfg.APIBaseURL, cfg.URLSource, DefaultAPIBaseURL, SourceDefault)
	}
	if cfg.Token != "" || cfg.TokenSource != "" {
		t.Errorf("TokenSource = %q, want empty: the environment reached the resolver", cfg.TokenSource)
	}
}
