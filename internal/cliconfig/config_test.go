package cliconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stump-wtf/cairn/internal/cliexit"
)

func writeConfigFile(t *testing.T, dir, contents string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(contents), perm); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func TestResolveDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Resolve(Options{ConfigPath: filepath.Join(dir, "missing.toml")})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.APIBaseURL != DefaultAPIBaseURL {
		t.Errorf("APIBaseURL = %q, want default %q", cfg.APIBaseURL, DefaultAPIBaseURL)
	}
	if cfg.URLSource != SourceDefault {
		t.Errorf("URLSource = %q, want %q", cfg.URLSource, SourceDefault)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty", cfg.Token)
	}
}

func TestResolvePrecedenceFileThenEnvThenFlag(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `url = "https://from-file.example"
token = "file-token"
`, 0o600)

	// File alone.
	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve (file only): %v", err)
	}
	if cfg.APIBaseURL != "https://from-file.example" || cfg.URLSource != SourceFile {
		t.Errorf("file precedence: got %q/%q", cfg.APIBaseURL, cfg.URLSource)
	}
	if cfg.Token != "file-token" || cfg.TokenSource != SourceFile {
		t.Errorf("file token: got %q/%q", cfg.Token, cfg.TokenSource)
	}

	// Env overrides file.
	t.Setenv("CAIRN_URL", "https://from-env.example")
	t.Setenv("CAIRN_TOKEN", "env-token")
	cfg, err = Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve (env over file): %v", err)
	}
	if cfg.APIBaseURL != "https://from-env.example" || cfg.URLSource != SourceEnv {
		t.Errorf("env precedence: got %q/%q", cfg.APIBaseURL, cfg.URLSource)
	}
	if cfg.Token != "env-token" || cfg.TokenSource != SourceEnv {
		t.Errorf("env token: got %q/%q", cfg.Token, cfg.TokenSource)
	}

	// Flag overrides env.
	cfg, err = Resolve(Options{
		ConfigPath:     path,
		FlagAPIBaseURL: "https://from-flag.example",
		FlagToken:      "flag-token",
	})
	if err != nil {
		t.Fatalf("Resolve (flag over env): %v", err)
	}
	if cfg.APIBaseURL != "https://from-flag.example" || cfg.URLSource != SourceFlag {
		t.Errorf("flag precedence: got %q/%q", cfg.APIBaseURL, cfg.URLSource)
	}
	if cfg.Token != "flag-token" || cfg.TokenSource != SourceFlag {
		t.Errorf("flag token: got %q/%q", cfg.Token, cfg.TokenSource)
	}
}

func TestResolveMissingConfigFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Resolve(Options{ConfigPath: filepath.Join(dir, "does-not-exist.toml")})
	if err != nil {
		t.Fatalf("Resolve: unexpected error for missing file: %v", err)
	}
	if cfg.APIBaseURL != DefaultAPIBaseURL {
		t.Errorf("APIBaseURL = %q, want default", cfg.APIBaseURL)
	}
}

func TestResolveMalformedFileIsUsageError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `not valid toml [[[`, 0o600)

	_, err := Resolve(Options{ConfigPath: path})
	if err == nil {
		t.Fatal("Resolve: want error for malformed config file")
	}
	if !errors.Is(err, cliexit.ErrUsage) {
		t.Errorf("Resolve error = %v, want wrapping cliexit.ErrUsage", err)
	}
}

func TestResolveWorldReadableTokenFileIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, `token = "leaky"`, 0o644)

	_, err := Resolve(Options{ConfigPath: path})
	if err == nil {
		t.Fatal("Resolve: want error for a world-readable token file")
	}
	if !errors.Is(err, cliexit.ErrUsage) {
		t.Errorf("Resolve error = %v, want wrapping cliexit.ErrUsage", err)
	}
}

func TestResolveWorldReadableFileWithoutTokenIsFine(t *testing.T) {
	dir := t.TempDir()
	// URL-only files are not sensitive; permissive mode should not block them.
	path := writeConfigFile(t, dir, `url = "https://cairn.example"`, 0o644)

	cfg, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if cfg.APIBaseURL != "https://cairn.example" {
		t.Errorf("APIBaseURL = %q", cfg.APIBaseURL)
	}
}

func TestValidateRejectsNonHTTPS(t *testing.T) {
	cfg := &Config{APIBaseURL: "http://cairn.example"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate: want error for non-HTTPS non-localhost URL")
	} else if !errors.Is(err, cliexit.ErrUsage) {
		t.Errorf("Validate error = %v, want wrapping cliexit.ErrUsage", err)
	}
}

func TestValidateAllowsLocalhostHTTP(t *testing.T) {
	for _, u := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		cfg := &Config{APIBaseURL: u}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected error: %v", u, err)
		}
	}
}

func TestValidateRejectsMalformedURL(t *testing.T) {
	for _, u := range []string{"not a url", "://missing-scheme", ""} {
		cfg := &Config{APIBaseURL: u}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%q): want error", u)
		}
	}
}

func TestDefaultConfigPathIsUnderCairnDir(t *testing.T) {
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if filepath.Base(path) != "config.toml" {
		t.Errorf("DefaultConfigPath = %q, want basename config.toml", path)
	}
	if filepath.Base(filepath.Dir(path)) != "cairn" {
		t.Errorf("DefaultConfigPath = %q, want parent dir cairn", path)
	}
}
