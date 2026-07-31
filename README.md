# Cairn

**AI-native artifact sharing.** A pastebin / gist / requestbin for the agent era:
pipe anything in, get a shareable, agent-native link back — *pbcopy for cairn*.
Humans post from the CLI and web; agents read, create, comment, and react over MCP.
Every artifact is a short URL with provenance, reactions, comments, and a TTL.

```
cat checkout-web-audit.md | cairn      →  cairn.sh/9qz1a
```

## Share types

Markdown · Code · Image · File · Bundle (multi-file) · Webhook (live requestbin) ·
**Trace** (a whole agent run — an OTel-style span waterfall + activity stream).

## Surfaces

- **Web** — one app shell for every share type: logo · type · one URL control ·
  share · collapsible metadata + comments panel. Plus **the Bin**, your artifact
  listing.
- **CLI** (`cairn`) — pipe or add files, `cairn ls` TUI to browse the Bin.
- **MCP** — agents read/create/comment/react over MCP, authorized via MCP OAuth.

## CLI — install, login, push

The `cairn` CLI is a single static Go binary — a pure REST client of the core `/v1`
API, with no domain logic of its own (see [ADR-0003](docs/adrs/ADR-0003-triple-surface-parity-web-cli-mcp.md)).
Cross-platform release binaries aren't published yet (tracked for a future
`goreleaser` job); until then, install straight from the module:

```bash
go install github.com/joestump/cairn/cmd/cairn@latest
```

This puts a `cairn` binary in `$(go env GOPATH)/bin` (make sure that's on your
`PATH`). Building from a checkout instead:

```bash
git clone https://github.com/joestump/cairn && cd cairn
go build -o cairn ./cmd/cairn
```

Authenticate with a bearer token — mint one from your Cairn server's web Settings
page, or use one your deployment operator issued:

```bash
cairn login --token <token>       # or: echo "$TOKEN" | cairn login
✓ authorized as sam@stump.rocks · via API
```

Tokens are stored securely (the OS keychain when available, otherwise a `0600`
file under your config directory) — never logged, never printed. Then push:

```bash
cat notes.md | cairn              # pipe content in, get a link back
cairn report.pdf                  # or pass a file path
cairn add a.png b.log dump.sql    # push several files as one bundle

cairn --ttl 24h --title "incident notes" incident.md   # optional flags
```

By default `cairn` copies the resulting link to your clipboard (best-effort,
`--no-copy` to disable) and shows the server-assigned expiry and access policy —
the CLI displays these, it never decides them. Piped/non-interactive output prints
only the bare `cairn.sh/<id>` link, so it composes cleanly in scripts:

```bash
url=$(cat report.md | cairn)
```

See [SPEC-0008](docs/openspec/specs/cli/spec.md) for the full command surface,
exit-code taxonomy, and `--json` mode.

## Project docs

- **[docs/DESIGN.md](docs/DESIGN.md)** — the product design brief (source of truth).
- **[docs/adrs/](docs/adrs/)** — Architecture Decision Records (MADR).
- **[docs/openspec/specs/](docs/openspec/specs/)** — OpenSpec specifications.

Built spec-first with the [`sdd`](https://github.com/joestump/claude-plugin-sdd)
workflow: decisions → specs → tracked issues.

## Status

Early design. The ADRs and specs in `docs/` define the intended architecture; the
GitHub issues track the build.
