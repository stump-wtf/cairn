// Package cliconfig resolves the cairn CLI's runtime configuration — the API
// base URL and bearer token — from flags, environment variables, and an
// on-disk config file, in that precedence order (SPEC-0008 "Configuration
// and Server Endpoint Resolution"). It validates the resolved base URL
// before any request is made and reports configuration errors as a usage
// error (cliexit.ErrUsage) rather than a network failure.
//
// Governing: SPEC-0008 (The cairn Command-Line Interface).
package cliconfig

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/joestump/cairn/internal/cliexit"
)

// DefaultAPIBaseURL is the built-in default server, used when no flag,
// environment variable, or config file sets one. It is the hosted deployment:
// a real, public, resolving host.
//
// It was previously https://cairn.sh, which has no DNS record at all, so a
// fresh `cairn login` failed for anyone who had not already been told to set
// CAIRN_URL or pass --url. That default survived because this comment cited
// "ADR-0005's public origin" — authority ADR-0005 does not grant. That ADR
// decides the identifier scheme (base62 alphabet, entropy, collision retry);
// it does not pick the host the service is deployed at, and no ADR or spec
// picks one. Nor is cairn.sh coming back: it was never acquired, so there is
// no future in which this line reverts.
const DefaultAPIBaseURL = "https://cairn.stump.wtf"

// Source names where a resolved field's value came from, exposed for
// --verbose diagnostics and tests. It never carries the value itself, so
// logging a Source is always safe even for the token field.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
	// SourceKeyring marks a token `cairn login` stored in the OS secret store
	// (SPEC-0008 "Secure Credential Storage": "MUST prefer the operating-
	// system secret store ... and, when none is available, MUST fall back to
	// a file"). See credential.go.
	SourceKeyring Source = "keyring"
)

// Config is the fully-resolved CLI configuration.
type Config struct {
	APIBaseURL string
	Token      string

	URLSource   Source
	TokenSource Source
}

// fileConfig is the on-disk TOML shape at ~/.config/cairn/config.toml:
//
//	url = "https://cairn.stump.wtf"
//	token = "..."
type fileConfig struct {
	URL   string `toml:"url"`
	Token string `toml:"token"`
}

// Options carries the explicit flag values (empty string = flag not
// passed) and lets tests point at an alternate config file path.
type Options struct {
	FlagAPIBaseURL string
	FlagToken      string

	// ConfigPath overrides the default ~/.config/cairn/config.toml. Tests
	// set this; production callers leave it empty.
	ConfigPath string
}

// DefaultConfigPath returns ~/.config/cairn/config.toml, honoring
// os.UserConfigDir so it follows XDG_CONFIG_HOME on Linux.
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cliconfig: resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "cairn", "config.toml"), nil
}

// Resolve applies flag → env (CAIRN_URL, CAIRN_TOKEN) → config file →
// built-in default, in that precedence order, then validates the result.
// A malformed config file, an unreadable-but-present config file, or an
// invalid resolved base URL all return an error wrapping cliexit.ErrUsage
// so the command layer exits with the usage-error code before any network
// call (SPEC-0008 "Configuration errors MUST be reported clearly and exit
// with the usage-error code before any network call").
func Resolve(opts Options) (*Config, error) {
	cfg := &Config{APIBaseURL: DefaultAPIBaseURL, URLSource: SourceDefault}

	path := opts.ConfigPath
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return nil, err
		}
	}

	fc, err := readFileConfig(path)
	if err != nil {
		return nil, err
	}
	if fc != nil {
		if fc.URL != "" {
			cfg.APIBaseURL = fc.URL
			cfg.URLSource = SourceFile
		}
		if fc.Token != "" {
			cfg.Token = fc.Token
			cfg.TokenSource = SourceFile
		}
	}

	if v := strings.TrimSpace(os.Getenv("CAIRN_URL")); v != "" {
		cfg.APIBaseURL = v
		cfg.URLSource = SourceEnv
	}
	if v := strings.TrimSpace(os.Getenv("CAIRN_TOKEN")); v != "" {
		cfg.Token = v
		cfg.TokenSource = SourceEnv
	}

	if opts.FlagAPIBaseURL != "" {
		cfg.APIBaseURL = opts.FlagAPIBaseURL
		cfg.URLSource = SourceFlag
	}
	if opts.FlagToken != "" {
		cfg.Token = opts.FlagToken
		cfg.TokenSource = SourceFlag
	}

	// No flag, env, or config-file token: try the OS keyring last, keyed on
	// the FINAL resolved API base URL (so a token stored for one server is
	// never handed to another) — `cairn login`'s preferred storage location
	// when the OS secret store is available (SPEC-0008 "Keychain-backed
	// storage"). A miss (nothing stored, or no secret-service reachable) is
	// silently ignored: the CLI simply reports not-authenticated, same as
	// today.
	if cfg.Token == "" {
		if tok, err := keyringLookup(cfg.APIBaseURL); err == nil && tok != "" {
			cfg.Token = tok
			cfg.TokenSource = SourceKeyring
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readFileConfig reads and parses path, returning (nil, nil) when the file
// does not exist — that is not an error, just "no file configured this
// field." A present-but-unreadable or malformed file IS an error: a config
// file the user thinks is in effect must not be silently ignored.
//
// The token field is only honored when the file is not group/other
// readable (mode&0077 == 0), mirroring SPEC-0008 "Secure Credential
// Storage" ("MUST be created with 0600 permissions and MUST NOT be
// world-readable") even though full OS-keychain storage lands with
// `cairn login` (#21): a config file the user hand-edited with a token in
// it is held to the same at-rest bar.
func readFileConfig(path string) (*fileConfig, error) {
	fc, info, err := loadFileConfigRaw(path)
	if err != nil || fc == nil {
		return fc, err
	}

	if fc.Token != "" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: cliconfig: %s contains a token but is not 0600 (mode %s); chmod 600 it or remove the token",
			cliexit.ErrUsage, path, info.Mode().Perm())
	}

	return fc, nil
}

// loadFileConfigRaw reads and parses path without the 0600 permission check
// readFileConfig layers on top — credential.go's SaveCredential/
// DeleteCredential use this directly so a pre-existing, insecurely-
// permissioned config file can still be read, amended, and rewritten with
// correct 0600 permissions (rather than being permanently stuck, unreadable,
// because the very check meant to protect it also blocks fixing it). It
// returns (nil, nil, nil) when the file does not exist.
func loadFileConfigRaw(path string) (*fileConfig, os.FileInfo, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: cliconfig: stat %s: %v", cliexit.ErrUsage, path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: cliconfig: read %s: %v", cliexit.ErrUsage, path, err)
	}

	var fc fileConfig
	if _, err := toml.Decode(string(data), &fc); err != nil {
		return nil, nil, fmt.Errorf("%w: cliconfig: parse %s: %v", cliexit.ErrUsage, path, err)
	}

	return &fc, info, nil
}

// localDevHosts are the hostnames the HTTPS requirement is waived for
// (SPEC-0008 "an explicit localhost override for development").
var localDevHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
}

// Validate checks that APIBaseURL is a well-formed absolute URL using
// HTTPS, except for the allowed localhost development override.
func (c *Config) Validate() error {
	u, err := url.Parse(c.APIBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%w: invalid API base URL %q", cliexit.ErrUsage, c.APIBaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: API base URL %q must use http or https", cliexit.ErrUsage, c.APIBaseURL)
	}
	if u.Scheme != "https" && !localDevHosts[u.Hostname()] {
		return fmt.Errorf("%w: API base URL %q must use https (only localhost is exempt for development)",
			cliexit.ErrUsage, c.APIBaseURL)
	}
	return nil
}
