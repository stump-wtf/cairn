---
title: Surfaces
sidebar_position: 2
slug: /surfaces
---

# Surfaces

Cairn is one core service behind three clients — **web**, **CLI**, and **MCP** —
with parity on the core operations: create, read, list, comment, react, share
(see [ADR-0003](../decisions/ADR-0003.md)).

## Web — the app shell

Every share type renders in the **same shell**, so the chrome never changes shape:

- a header — **logo · type badge · title · one URL control** (with `copy` and an
  `◆ mcp` affordance) · **Share**;
- a **collapsible** right-hand metadata + comments panel;
- a type-specific **body** (markdown, code, image, file, bundle tabs, the webhook
  inspector, or the trace waterfall + stream).

### The Bin

The Bin is your artifact listing — *"what an agent has dropped for you."* Rows carry
the type badge, title, provenance, and reaction/comment counts
(`claude · via mcp · 1d · 💬 2 · 👀 3`). Same listing, whether you browse it on the
web or in the terminal.

See [SPEC-0001](../specs/web-app-shell-and-bin/index.md).

## CLI — `cairn`

*pbcopy for cairn.* A single static Go binary; the TUI is Bubble Tea. No release
binaries yet — install straight from the module:

```bash
go install github.com/joestump/cairn/cmd/cairn@latest
```

Authenticate with a bearer token minted from your server's Settings page (the
OAuth 2.1 + PKCE browser flow lands in a follow-up), then push:

```bash
cairn login --token <token>
✓ authorized as sam@stump.rocks · via API

# pipe anything in, get a link back
cat checkout-web-audit.md | cairn

# push many files at once as a bundle
cairn add audit.md parse_ledger.py Movies.png dump.sql.gz

# optional flags: expiry hint and a display title
cairn --ttl 24h --title "incident notes" incident.md

# browse the Bin as a keyboard-driven TUI
cairn ls        # ↑/k up · ↓/j down · / filter · enter open · s share · q quit
```

Tokens are stored securely (OS keychain, or a `0600` file as a fallback) and
never logged. See
[SPEC-0008](../specs/cli/index.md).

## MCP — the agent surface

Agents get the same operations humans do, over MCP: **read artifacts, create & push
new artifacts, comment & react** — plus read access to live webhook and trace
streams (`mcp://cairn/hook/<id>`).

### OAuth consent

Access is granted through **MCP OAuth** (OAuth 2.1, authorization-code + PKCE). The
consent screen grants exactly three scopes:

> **Claude Desktop wants to connect to your Cairn workspace over MCP**
> This will allow Claude to:
> - Read artifacts you can access
> - Create & push new artifacts
> - Comment & react on your behalf
>
> *Connected over MCP · revoke anytime in settings.*

Agents act as the human (subject) with a distinct model identity (actor), and never
exceed the human's reach. See
[ADR-0004](../decisions/ADR-0004.md)
and [SPEC-0007](../specs/mcp-server-and-oauth/index.md).
