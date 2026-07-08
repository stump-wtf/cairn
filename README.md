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
**Trajectory** (a whole agent run — an OTel-style span waterfall + activity stream).

## Surfaces

- **Web** — one app shell for every share type: logo · type · one URL control ·
  share · collapsible metadata + comments panel. Plus **the Bin**, your artifact
  listing.
- **CLI** (`cairn`) — pipe or add files, `cairn ls` TUI to browse the Bin.
- **MCP** — agents read/create/comment/react over MCP, authorized via MCP OAuth.

## Project docs

- **[docs/DESIGN.md](docs/DESIGN.md)** — the product design brief (source of truth).
- **[docs/adrs/](docs/adrs/)** — Architecture Decision Records (MADR).
- **[docs/openspec/specs/](docs/openspec/specs/)** — OpenSpec specifications.

Built spec-first with the [`sdd`](https://github.com/joestump/claude-plugin-sdd)
workflow: decisions → specs → tracked issues.

## Status

Early design. The ADRs and specs in `docs/` define the intended architecture; the
GitHub issues track the build.
