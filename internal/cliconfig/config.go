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

// DefaultAPIBaseURL is the built-in default server (ADR-0005's public
// origin) used when no flag, environment variable, or config file sets one.
const DefaultAPIBaseURL = "https://cairn.sh"

// Source names where a resolved field's value came from, exposed for
// --verbose diagnostics and tests. It never carries the value itself, so
// logging a Source is always safe even for the token field.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
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
//	url = "https://cairn.sh"
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
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: cliconfig: stat %s: %v", cliexit.ErrUsage, path, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cliconfig: read %s: %v", cliexit.ErrUsage, path, err)
	}

	var fc fileConfig
	if _, err := toml.Decode(string(data), &fc); err != nil {
		return nil, fmt.Errorf("%w: cliconfig: parse %s: %v", cliexit.ErrUsage, path, err)
	}

	if fc.Token != "" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: cliconfig: %s contains a token but is not 0600 (mode %s); chmod 600 it or remove the token",
			cliexit.ErrUsage, path, info.Mode().Perm())
	}

	return &fc, nil
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
