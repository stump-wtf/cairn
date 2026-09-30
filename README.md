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

`cairn` is one static Go binary. As a client it is a pure REST client of the core
`/v1` API, with no domain logic of its own (see [ADR-0003](docs/adrs/ADR-0003-triple-surface-parity-web-cli-mcp.md)).
Since v0.3.0 it is also the server: `cairn serve` replaces the old `cairnd`
binary (see [ADR-0031](docs/adrs/ADR-0031-one-cairn-binary-the-server-is-cairn-serve.md)).

### Install

With Homebrew, on macOS or Linux:

```bash
brew install stump-wtf/tap/cairn
```

Or download an archive for your platform from
[GitHub Releases](https://github.com/stump-wtf/cairn/releases), or install from
the module:

```bash
go install github.com/stump-wtf/cairn/cmd/cairn@latest
```

That puts `cairn` in `$(go env GOPATH)/bin`, so make sure that is on your `PATH`.
To build from a checkout instead:

```bash
git clone https://github.com/stump-wtf/cairn && cd cairn
go build -o cairn ./cmd/cairn
```

### Log in

Authenticate with a bearer token. Mint a personal access token from your Cairn
server's web Settings page, or use one your deployment operator issued:

```bash
cairn login --token <token>       # or: echo "$TOKEN" | cairn login
✓ authorized as sam@stump.rocks · via API
cairn whoami                      # check who you are
```

A bare `cairn login` prompts for the token with input hidden. Tokens are stored in
the OS keyring when one is reachable (macOS Keychain, Secret Service on Linux),
otherwise in a `0600` file under your config directory. They are never logged or
printed. `cairn logout` deletes the stored copy.

### Push

```bash
cat notes.md | cairn              # pipe content in, get a link back
cairn report.pdf                  # or pass a file path
cairn add a.png b.log dump.sql    # push several files as one bundle

cairn --ttl 24h --title "incident notes" incident.md   # optional flags
```

By default `cairn` copies the resulting link to your clipboard (best-effort,
`--no-copy` to disable) and shows the server-assigned expiry and access policy —
the CLI displays these, it never decides them. Piped/non-interactive output prints
only the bare `cairn.stump.wtf/<id>` link, so it composes cleanly in scripts:

```bash
url=$(cat report.md | cairn)
```

See the [CLI reference](docs/openspec/specs/cli/cli-reference.md) for every
command, flag and exit code, and [SPEC-0008](docs/openspec/specs/cli/spec.md) for
the requirements behind them.

### Run the server

`cairn serve` runs the web app, the `/v1` API, the SSE streams and the MCP server
in one process. It takes no flags; its whole configuration is the `CAIRN_*`
environment. The [self-hosting guide](https://cairn.stump.wtf/docs/guides/self-hosting/)
lists the variables and walks through a full deployment.

Upgrading from `cairnd` changes only the command name: `cairn serve` reads the same
variables with the same meanings. The container image still starts through a
`cairnd` shim that runs `cairn serve`, so container deployments need no change.

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
releases ship as the `cairn` binary (Homebrew and GitHub Releases) and the
container image `ghcr.io/stump-wtf/cairn`, currently the `v0.3.x` line. The ADRs
and specs in `docs/` are the design record the code follows.

## Reporting bugs

Report bugs in [GitHub Issues](https://github.com/stump-wtf/cairn/issues). The bug
form asks for the version, deployment and surface we need to reproduce it.
Report a vulnerability privately instead, as [SECURITY.md](SECURITY.md) describes.

Development happens on a private Gitea instance, and this GitHub repository is a
read-only mirror of it. Bugs filed here are carried over to the internal tracker.
