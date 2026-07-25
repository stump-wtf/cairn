package cliconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain forces every test in this package onto zalando/go-keyring's
// in-memory mock, defaulting to "no secret service available" — the
// deterministic equivalent of a headless CI box with no dbus session or
// libsecret, so tests never depend on (or leak into) whatever real OS
// keyring happens to be reachable on the machine running `go test`.
// Individual tests that need the "keyring IS available" branch call
// keyring.MockInit() themselves and restore the error mock via
// t.Cleanup so later tests see the default again.
func TestMain(m *testing.M) {
	keyring.MockInitWithError(errors.New("keyring: no secret service in test"))
	os.Exit(m.Run())
}

// withMockKeyring switches to a fresh in-memory keyring for the duration of
// the test, restoring the package-wide error mock afterward.
func withMockKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(func() {
		keyring.MockInitWithError(errors.New("keyring: no secret service in test"))
	})
}

func TestSaveCredentialFallsBackToFileWhenKeyringUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	src, err := SaveCredential(path, "https://cairn.example", "tok-file-fallback")
	if err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if src != SourceFile {
		t.Fatalf("source = %q, want %q", src, SourceFile)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %s, want 0600", info.Mode().Perm())
	}

	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Token != "tok-file-fallback" || cfg.TokenSource != SourceFile {
		t.Errorf("Token/%q = %q/%q, want tok-file-fallback/file", cfg.TokenSource, cfg.Token, cfg.TokenSource)
	}
	if cfg.APIBaseURL != "https://cairn.example" {
		t.Errorf("APIBaseURL = %q", cfg.APIBaseURL)
	}
}

func TestSaveCredentialPrefersKeyringAndNeverWritesTokenToFile(t *testing.T) {
	withMockKeyring(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	src, err := SaveCredential(path, "https://cairn.example", "tok-keyring")
	if err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if src != SourceKeyring {
		t.Fatalf("source = %q, want %q", src, SourceKeyring)
	}

	// The file must carry the URL (so later invocations resolve the same
	// server with no flags) but MUST NOT contain the plaintext token.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if strings.Contains(string(raw), "tok-keyring") {
		t.Fatalf("config file leaked the token: %s", raw)
	}

	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Token != "tok-keyring" || cfg.TokenSource != SourceKeyring {
		t.Errorf("Token/Source = %q/%q, want tok-keyring/keyring", cfg.Token, cfg.TokenSource)
	}
}

func TestResolveKeyringFallbackKeyedByAPIBaseURL(t *testing.T) {
	withMockKeyring(t)
	if err := keyring.Set(keyringService, "https://a.example", "tok-a"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}
	if err := keyring.Set(keyringService, "https://b.example", "tok-b"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}

	cfg, err := Resolve(Options{ConfigPath: filepath.Join(t.TempDir(), "missing.toml"), FlagAPIBaseURL: "https://b.example"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Token != "tok-b" || cfg.TokenSource != SourceKeyring {
		t.Errorf("Token/Source = %q/%q, want tok-b/keyring (keyed on the resolved URL)", cfg.Token, cfg.TokenSource)
	}
}

func TestResolveFlagTokenWinsOverKeyring(t *testing.T) {
	withMockKeyring(t)
	if err := keyring.Set(keyringService, DefaultAPIBaseURL, "tok-keyring"); err != nil {
		t.Fatalf("keyring.Set: %v", err)
	}

	cfg, err := Resolve(Options{ConfigPath: filepath.Join(t.TempDir(), "missing.toml"), FlagToken: "tok-flag"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Token != "tok-flag" || cfg.TokenSource != SourceFlag {
		t.Errorf("Token/Source = %q/%q, want tok-flag/flag (flag beats keyring)", cfg.Token, cfg.TokenSource)
	}
}

func TestDeleteCredentialClearsFileFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, err := SaveCredential(path, "https://cairn.example", "tok-to-remove"); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	removed, err := DeleteCredential(path, "https://cairn.example")
	if err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true")
	}

	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve after delete: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q after logout, want empty", cfg.Token)
	}
	// The URL survives logout — only the credential is forgotten.
	if cfg.APIBaseURL != "https://cairn.example" {
		t.Errorf("APIBaseURL = %q after logout, want it preserved", cfg.APIBaseURL)
	}
}

func TestDeleteCredentialClearsKeyring(t *testing.T) {
	withMockKeyring(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, err := SaveCredential(path, "https://cairn.example", "tok-keyring"); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	removed, err := DeleteCredential(path, "https://cairn.example")
	if err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true")
	}

	if _, err := keyring.Get(keyringService, "https://cairn.example"); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("keyring.Get after delete: err = %v, want ErrNotFound", err)
	}

	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve after delete: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q after logout, want empty", cfg.Token)
	}
}

func TestDeleteCredentialWithNothingStoredIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	removed, err := DeleteCredential(path, "https://cairn.example")
	if err != nil {
		t.Fatalf("DeleteCredential on empty state: %v", err)
	}
	if removed {
		t.Error("removed = true, want false (nothing was stored)")
	}
}
