# Cairn

**AI-native artifact sharing.** A pastebin / gist / requestbin for the agent era:
pipe anything in, get a shareable, agent-native link back — *pbcopy for cairn*.
Humans post from the CLI and web; agents read, create, comment, and react over MCP.
Every artifact is a short URL with provenance, reactions, comments, and a TTL.

```
cat checkout-web-audit.md | cairn      →  cairn.stump.wtf/9qz1a
```

## Share types

Markdown · Code · Image · File · Bundle (multi-file) · Webhook (live requestbin) ·
**Trace** (a whole agent run — an OTel-style span waterfall + activity stream).

## Surfaces

- **Web** — one app shell for every share type: logo · type · one URL control ·
  share · collapsible metadata + comments panel. Plus **the Bin**, your artifact
  listing.
- **CLI** (`cairn`) — pipe or add files, log in, and check who you are. A
  `cairn ls` TUI to browse the Bin is planned, not shipped.
- **MCP** — agents read/create/comment/react over MCP, authorized via MCP OAuth.

## CLI — install, login, push

The `cairn` CLI is a single static Go binary — a pure REST client of the core `/v1`
API, with no domain logic of its own (see [ADR-0003](docs/adrs/ADR-0003-triple-surface-parity-web-cli-mcp.md)).
`cairn` is now a monolithic binary: all functionality, including the `cairn serve`
server (formerly `cairnd` binary), is in one executable (ADR-0031).

### Installation

**Homebrew (recommended for macOS/Linux):**

```bash
brew install stump-wtf/tap/cairn
```

This provides pre-built binaries for the latest release (`v0.3.0` and later).

**Other platforms:** If Homebrew is not available, install from the Go module:

```bash
go install github.com/stump-wtf/cairn/cmd/cairn@latest
```

This puts a `cairn` binary in `$(go env GOPATH)/bin` (make sure that's on your
`PATH`).

**Building from source:**

```bash
# For releases, check GitHub releases for tagged versions
# https://github.com/stump-wtf/cairn/releases

# For development, build from the repository
git clone https://gitea.stump.rocks/stump.wtf/cairn && cd cairn
go build -o cairn ./cmd/cairn
```

### Upgrading from `cairnd` (standalone server)

If you were previously running a separate `cairnd` binary, you can migrate to the
single `cairn` binary by installing the Homebrew package or building from source:

```bash
# Install the monolithic cairn binary
brew install stump-wtf/tap/cairn

# Run the server with the same environment variables
CAIRN_DATABASE_URL="$CAIRND_DATABASE_URL" \
CAIRN_S3_ENDPOINT="$CAIRND_S3_ENDPOINT" \
CAIRN_S3_BUCKET="$CAIRND_S3_BUCKET" \
cairn serve
```

The Docker container continues to ship a `cairnd` shim that calls `cairn serve` for
backwards compatibility — no migration is required for containerized deployments.

### Authentication and usage

Authenticate with a bearer token — mint one from your Cairn server's web Settings
page, or use one issued by your deployment operator:

```bash
cairn login       # Interactive OAuth flow (recommended)
# or
cairn login --token <token>   # Use a presigned token
# or
echo "$TOKEN" | cairn login   # Pipe token securely

# Confirm authentication
cairn whoami

# Token is stored securely (OS keychain on macOS, Linux file under
# ~/.local/share/cairn/ or $XDG_DATA_HOME/cairn/, Windows Credential Manager).
# Tokens are never logged or printed.
```

Then use the CLI:

```bash
# Create single artifacts
cat notes.md | cairn              # pipe content in, get a link back
cairn report.pdf                  # or pass a file path

# Create bundles (multiple files)
cairn add a.png b.log dump.sql    # push several files as one bundle

# Customize with optional flags
cairn --ttl 24h --title "incident notes" incident.md

# Machine-readable output
cat report.md | cairn --json

# Disable clipboard copy (useful in scripts)
cairn report.pdf --no-copy
```

By default, `cairn` copies the resulting link to your clipboard and prints the
server-assigned expiry. Non-interactive output contains only the bare link for
composition in scripts:

```bash
url=$(cat report.md | cairn)
echo $url    # outputs: cairn.sh/abc123
```

### Server deployment

To run the Cairn server (web app, API, MCP server) locally:

```bash
# Configure environment variables
export CAIRN_DATABASE_URL="postgres://user:pass@localhost:5432/cairn"
export CAIRN_S3_ENDPOINT="https://s3.stump.rocks:9000"
export CAIRN_S3_BUCKET="cairn"
export CAIRN_JWT_SECRET="your-strong-secret"

# Start the server
cairn serve

# On ctrl+C, the server shuts down gracefully
```

See the [server deployment guide](docs/server/deployment.md) for production
configuration.

### CLI reference

For full command reference, subcommand flags, exit codes, and troubleshooting,
see [SPEC-0008 CLI reference](docs/openspec/specs/cli/cli-reference.md).

## Self-hosting

Want your own cairn? The [self-hosting guide](https://cairn.stump.wtf/docs/guides/self-hosting/)
takes you from nothing to a running instance: Postgres + any S3-compatible
store, a pasteable compose file, OIDC sign-in, and a verified end-to-end loop.
The image is `ghcr.io/stump-wtf/cairn`.

## Project docs

- **[docs/DESIGN.md](docs/DESIGN.md)** — the product design brief (source of truth).
- **[docs/adrs/](docs/adrs/)** — Architecture Decision Records (MADR).
- **[docs/openspec/specs/](docs/openspec/specs/)** — OpenSpec specifications.

Built spec-first with the [`sdd`](https://github.com/joestump/claude-plugin-sdd)
workflow: decisions → specs → tracked issues.

## Status

Cairn is running and released. https://cairn.stump.wtf is a live instance, and
releases ship as the container image `ghcr.io/stump-wtf/cairn`, currently the
`v0.1.x` line. The ADRs and specs in `docs/` are the design record the code follows.

## Reporting bugs

Report bugs in [GitHub Issues](https://github.com/stump-wtf/cairn/issues). The bug
form asks for the version, deployment and surface we need to reproduce it.
Report a vulnerability privately instead, as [SECURITY.md](SECURITY.md) describes.

Development happens on a private Gitea instance, and this GitHub repository is a
read-only mirror of it. Bugs filed here are carried over to the internal tracker.
