# Cairn CLI Reference

This document describes all `cairn` commands, flags, and behaviors. It is generated from `cairn help` and subcommand help text.

## Commands

### `cairn` [FILE...] [FLAGS]

Create a single artifact or bundle from stdin/file(s).

**Default behavior (pipe or single file):** Read from stdin or the first file argument and create one artifact via `POST /v1/artifacts`.

**Bundle behavior:** With multiple files (`cairn a.png b.log c.sql`), create one bundle containing all files.

**Flags:**

| Flag | Type | Description |
|------|------|-------------|
| `--concurrency` | int | Bounded worker pool size for `cairn add`'s local file preparation (default: 4) |
| `-h, --help` | flag | Show help |
| `--json` | flag | Emit machine-readable JSON output instead of human-readable text |
| `--no-copy` | flag | Never copy the resulting link to the clipboard |
| `--redact` | flag | Store detected credentials as `[REDACTED]` instead of refusing the upload |
| `--tag` | repeated | Attach a routing tag; repeatable or comma-separated (e.g. `--tag handoff --tag lane:auto`) |
| `--title` | string | Optional display title for the artifact/bundle |
| `--token` | string | Bearer token (overridden by `CAIRN_TOKEN` env var) |
| `--ttl` | duration | Request an expiry (e.g. `"24h"`, `"7d"`); the server decides whether to honor it |
| `--type` | string | Override the detected Content-Type |
| `--url` | string | API base URL (overridden by `CAIRN_URL` env var; default: `https://cairn.stump.wtf`) |
| `-v, --verbose` | flag | Emit structured diagnostic output to stderr (tokens redacted) |
| `--version` | flag | Display the cairn version |

**Environment variables:**

- `CAIRN_TOKEN` - Bearer token for authentication (or use `cairn login`)
- `CAIRN_URL` - Base URL of the Cairn API server

**Exit codes:**

- `0` - Success; link printed to stdout
- `1` - General error
- `2` - Authentication/authorization failure
- `3` - Input validation or server rejection

**Examples:**

```bash
# Pipe content, get link printed and copied to clipboard
cat incident.md | cairn

# Create artifact from a file
cairn report.pdf

# Create bundle from multiple files
cairn add screenshot.png error.log database.sql

# Set custom TTL, title, and clipboard flag
cairn --ttl 24h --title "meeting notes" notes.md

# Machine-readable output
cat report.md | cairn --json

# Disable clipboard copy
cairn report.pdf --no-copy
```

### `cairn serve`

Run the Cairn server in this process.

**Note:** This is the former `cairnd` binary, folded into `cairn` as a CLI subcommand (ADR-0031). It provides the web app, `/v1` REST API, SSE streams, and MCP server from a single binary.

**Configuration:**

All configuration is provided via environment variables — the same variables `cairnd` read, with the same meanings:

- `CAIRN_HTTP_ADDR` - HTTP listen address for the web app and API (default: `:8080`)
- `CAIRN_DATABASE_URL` - PostgreSQL connection string
- `CAIRN_S3_ENDPOINT` - S3-compatible storage endpoint (e.g. for Garage S3)
- `CAIRN_S3_BUCKET` - S3 bucket name
- `CAIRN_S3_ACCESS_KEY` - S3 access key
- `CAIRN_S3_SECRET_KEY` - S3 secret key
- `CAIRN_DOMAIN` - Domain used for link generation
- `CAIRN_JWT_SECRET` - JWT secret for session tokens
- `CAIRN_MCP_PORT` - MCP server listen port (default: 3033)
- `CAIRN_DYNAMIC_HOST` - Dynamic host for task subdomains

**Flags:**

Same flags as `cairn` (global flags apply to both `cairn` and `cairn serve`).

**Important:** Although there are command-line flags, a production deployment should use environment variables exclusively; mixing the two is not recommended.

**Usage:**

```bash
# Start the server with all required environment variables configured
CAIRN_DATABASE_URL="postgres://..." CAIRN_S3_ENDPOINT="s3.stump.rocks:9000" CAIRN_S3_BUCKET="cairn" cairn serve

# Start with custom HTTP address
CAIRN_HTTP_ADDR=":3000" cairn serve

# Start with high verbosity
cairn serve -v
```

**Termination:**

- Send `SIGINT` (Ctrl+C) to gracefully shutdown the server

**Upgrade from `cairnd`:**

If you were previously running the standalone `cairnd` binary, you can migrate to `cairn serve` by installing Homebrew or building from source:

```bash
# Homebrew
brew install stump-wtf/tap/cairn

# or build (GCC/Clang)
go install github.com/stump-wtf/cairn/cmd/cairn@latest

# Run with the same environment variables
CAIRN_DATABASE_URL="$CAIRND_DATABASE_URL" \
CAIRN_S3_ENDPOINT="$CAIRND_S3_ENDPOINT" \
CAIRN_S3_BUCKET="$CAIRND_S3_BUCKET" \
CAIRN_JWT_SECRET="$CAIRND_JWT_SECRET" \
CAIRN_HTTP_ADDR="$CAIRND_HTTP_ADDR" \
cairn serve
```

The container image continues to ship a `cairnd` shim binary that calls `cairn serve` for compatibility with existing deployments.

### `cairn login`

Authenticate with a Cairn server using OAuth 2.1 authorization code flow.

This command performs a PKCE-authenticated OAuth login using the provider configured in your Cairn server. It prints a success or failure message and stores the resulting bearer token securely (OS keychain where available, otherwise in a `0600` file).

**No arguments required.**

**Usage:**

```bash
cairn login

# Output:
# ✓ authorized as joe@stump.rocks · via API
```

**Supported providers:** See your Cairn server's OAuth configuration. By default the server uses the OpenBao/HashiCorp Vault OIDC provider.

**Note:** This is the only command that executes without authentication — it authorizes you and gets the credentials needed for all other CLI commands.

### `cairn logout`

Revoke this machine's session with the Cairn server (RFC 7009).

This sends a token revocation request to the server and locally removes any stored credentials (or clears the OS keychain on supported platforms).

**No arguments required.**

**Usage:**

```bash
cairn logout

# Output:
# ✓ logged out from joe@stump.rocks
```

### `cairn whoami`

Show the current authentication state, including who you're authenticated as and whether the token is valid.

**No arguments required.**

**Usage:**

```bash
cairn whoami

# Output:
# ✓ authorized as joe@stump.rocks · via API
# Token expires: 2026-10-06T21:57:13Z
```

### `cairn add FILE...`

Alias for `cairn` command with multiple file arguments (bundle creation).

Creates a single bundle artifact containing all specified files.

**Flags:** Same as `cairn` (global flags apply).

**Usage:**

```bash
cairn add screenshot.png error.log database.sql

# Set bundle metadata
cairn --title "incident dump" --ttl 7d add screenshot.png error.log database.sql
```

### `cairn completion [COMMAND]`

Generate shell autocompletion scripts for the cairn CLI.

**Arguments:**

- `[COMMAND]` - Optional: target shell (`bash`, `zsh`, `fish`)

**Usage:**

```bash
# Bash
cairn completion bash > ~/.config/bash_completion.d/cairn

# Zsh
cairn completion zsh > "${fpath[1]}/cairn"
compinit

# Fish
cairn completion fish > ~/.config/fish/completions/cairn.fish

# Fish, built-in install
cairn completion fish
```

### `cairn help [COMMAND]`

Show help about any cairn command or flag.

**Arguments:**

- `[COMMAND]` - Optional: specific command to show help for

**Usage:**

```bash
# Show global help
cairn help

# Show specific command help
cairn help serve
cairn help add
```

## Global Flags

All commands share the following flags:

| Flag | Description |
|------|-------------|
| `--concurrency` | Worker pool size for bundle creation (default: 4) |
| `--redact` | Store detected secrets as `[REDACTED]` instead of failing |
| `--tag TAG` | Attach routing tag(s) to the artifact |
| `--ttl TTL` | Request custom TTL for the artifact |
| `--url URL` | Cairn API base URL (default: `https://cairn.stump.wtf`) |

## Authentication and Security

### Token storage

Tokens are never logged or printed. They are stored securely:

- **macOS/iOS:** OS Keychain (using the system credential manager)
- **Linux:** A `0600` file in `~/.local/share/cairn/` or `$XDG_DATA_HOME/cairn/`
- **Windows:** Credential Manager

### PKCE for login

The `cairn login` command uses PKCE (Proof Key for Code Exchange) to authenticate with OAuth providers. This is more secure than the legacy authorization code flow and prevents authorization interception attacks.

### Redaction mode

With `--redact`, credentials detected in file content (passwords, API keys, tokens) are not refused — they are stored as `[REDACTED]`. This mode does not remove false positives and should be used with caution. The only safe value for production uploads is to keep secrets out of shared artifacts entirely.

## Environment variables

### Required (for `cairn serve`)

| Variable | Description | Example |
|----------|-------------|---------|
| `CAIRN_DATABASE_URL` | PostgreSQL connection string | `postgres://user:pass@localhost:5432/cairn` |
| `CAIRN_S3_ENDPOINT` | S3-compatible storage endpoint | `https://s3.stump.rocks:9000` |
| `CAIRN_S3_BUCKET` | S3 bucket name | `cairn` |
| `CAIRN_S3_ACCESS_KEY` | S3 access key | `minioadmin` |
| `CAIRN_S3_SECRET_KEY` | S3 secret key | `minioadmin` |
| `CAIRN_JWT_SECRET` | JWT secret for session tokens | Generate any strong secret |
| `CAIRN_DOMAIN` | Domain for link generation | `cairn.stump.wtf` |

### Optional

| Variable | Description | Default |
|----------|-------------|---------|
| `CAIRN_HTTP_ADDR` | HTTP listen address | `:8080` |
| `CAIRN_RESPONSE_LIMIT_MB` | Response size limit in MB | `100` |
| `CAIRN_PORT` | HTTP port (deprecated; use `CAIRN_HTTP_ADDR`) | `8080` |
| `CAIRN_CONCURRENCY` | Worker concurrency (deprecated) | `4` |
| `CAIRN_DYNAMIC_HOST` | Dynamic host for task subdomains | `cairn.stump.wtf` |
| `CAIRN_MCP_PORT` | MCP server port | `3033` |

### CLI runtime

| Variable | Description |
|----------|-------------|
| `CAIRN_TOKEN` | Bearer token for authentication (overrides interactive login) |
| `CAIRN_URL` | API base URL (overrides default) |

## Exit-code taxonomy

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | General error (parsing, network, or server error) |
| `2` | Authentication/authorization failure (invalid token, permission denied) |
| `3` | Input validation error (empty input, invalid file, unsupported type) |
| `4` | User aborted (Ctrl+C, quit in TUI) |

## Troubleshooting

### Authentication errors

```bash
# Clear stored credentials
rm ~/.local/share/cairn/token  # Linux only

# Or use logout to revoke from server
cairn logout

# Re-login
cairn login
```

### Clipboard not working

The clipboard logic uses the OS clipboard APIs; it may fail on:
- SSH sessions without X11 forwarding
- Wayland sessions with copypaste issues
- Headless environments with no clipboard server

Use `--no-copy` to suppress clipboard attempts entirely.

### Rate limiting

If you see `429 Too Many Requests`, the server is rate-limiting your requests. Wait a moment and retry, or avoid creating too many artifacts in quick succession.
