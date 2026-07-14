// Credential storage for `cairn login`/`cairn logout` (cairn#21, SPEC-0008
// "Secure Credential Storage"): prefer the OS secret store (macOS Keychain,
// the Secret Service/libsecret on Linux, Windows Credential Manager — all
// via github.com/zalando/go-keyring) and fall back to the 0600 config file
// only when no secret store is reachable. Storage is keyed on the resolved
// API base URL so tokens for different Cairn deployments never collide, and
// SaveCredential never leaves a plaintext copy in the file once the keyring
// accepted it.
package cliconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"

	"github.com/joestump/cairn/internal/cliexit"
)

// keyringService namespaces cairn's entries in the OS secret store from
// every other application's.
const keyringService = "cairn-cli"

// keyringLookup wraps keyring.Get, translating "nothing stored" and "no
// secret service available" (both keyring.ErrNotFound-shaped, or a hard
// error on a headless box with no Secret Service/dbus session) into a plain
// miss — Resolve treats either as "no keyring credential," never a fatal
// error, since the keyring is a best-effort convenience, not a requirement.
func keyringLookup(apiBaseURL string) (string, error) {
	return keyring.Get(keyringService, apiBaseURL)
}

// SaveCredential is what `cairn login` calls after the server has verified
// the candidate token (the whoami round trip): it persists apiBaseURL and
// token so a later invocation with no --token/CAIRN_TOKEN resolves them
// again. It prefers the OS keyring; only when the keyring is unavailable
// does it fall back to writing the token into the 0600 config file
// (SPEC-0008 "File fallback permissions"). The resolved API base URL is
// always written to the config file (flag/env override it per-invocation,
// but a saved default makes `cairn whoami`/`cairn add` work with no flags),
// regardless of which store won the token.
func SaveCredential(configPath, apiBaseURL, token string) (Source, error) {
	path, err := resolveConfigPath(configPath)
	if err != nil {
		return "", err
	}

	fc := fileConfig{}
	existing, _, err := loadFileConfigRaw(path)
	if err != nil {
		return "", err
	}
	if existing != nil {
		fc = *existing
	}
	fc.URL = apiBaseURL

	if err := keyring.Set(keyringService, apiBaseURL, token); err == nil {
		// The keyring took it: never also keep a plaintext copy on disk.
		fc.Token = ""
		if err := writeFileConfig(path, fc); err != nil {
			return "", err
		}
		return SourceKeyring, nil
	}

	fc.Token = token
	if err := writeFileConfig(path, fc); err != nil {
		return "", err
	}
	return SourceFile, nil
}

// DeleteCredential is what `cairn logout` calls: it removes the credential
// from wherever SaveCredential put it. It is idempotent — deleting an
// already-absent credential from either store is not an error, matching
// SPEC-0008's revoke-then-delete lifecycle where logout with no active
// session should not fail the command.
func DeleteCredential(configPath, apiBaseURL string) (removed bool, err error) {
	path, err := resolveConfigPath(configPath)
	if err != nil {
		return false, err
	}

	// Any keyring error — nothing was stored there (keyring.ErrNotFound) or
	// the secret store itself is unreachable — is swallowed: logout must
	// still be able to clear the file-based fallback even when the OS
	// secret store never held this credential (e.g. it was a file-fallback
	// login all along) or is misbehaving.
	if kerr := keyring.Delete(keyringService, apiBaseURL); kerr == nil {
		removed = true
	}

	existing, _, err := loadFileConfigRaw(path)
	if err != nil {
		return removed, err
	}
	if existing == nil || existing.Token == "" {
		return removed, nil
	}

	fc := *existing
	fc.Token = ""
	if err := writeFileConfig(path, fc); err != nil {
		return removed, err
	}
	return true, nil
}

// resolveConfigPath applies the same "explicit override, else
// DefaultConfigPath()" rule Resolve uses.
func resolveConfigPath(configPath string) (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	return DefaultConfigPath()
}

// writeFileConfig serializes fc as TOML to path with owner-only (0600)
// permissions (SPEC-0008 "File fallback permissions": "MUST be created with
// 0600 permissions and MUST NOT be world-readable"), creating the parent
// directory if needed. It forces the mode with an explicit Chmod after
// writing because os.WriteFile only applies its mode argument when it
// creates the file — a pre-existing file keeps its old (possibly looser)
// permissions otherwise.
func writeFileConfig(path string, fc fileConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: cliconfig: create config dir %s: %v", cliexit.ErrUsage, dir, err)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(fc); err != nil {
		return fmt.Errorf("%w: cliconfig: encode config: %v", cliexit.ErrUsage, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("%w: cliconfig: write config %s: %v", cliexit.ErrUsage, path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("%w: cliconfig: chmod config %s: %v", cliexit.ErrUsage, path, err)
	}
	return nil
}
