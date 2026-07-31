---
title: Share types
sidebar_position: 1
slug: /share-types
---

# Share types

A share type is the *kind* of artifact. It decides three things: the **viewer** that
renders the body, the **metadata panel** on the right, and the **annotation anchors**
you can react to or comment on. The set is **extensible** — traces were added
as a new type without disturbing the others (see
[ADR-0002](../decisions/ADR-0002.md)).

## <span class="chip chip-read">MD</span> Markdown

Rendered markdown with a table of contents. React under blocks and to the left of
bullets; select any text to drop a comment in the right margin. *Example:
`checkout-web-audit.md`.*

## <span class="chip chip-exec">PY</span> Code

Syntax-highlighted source with line numbers and a symbol outline. Comment a single
line or a selection. *Example: `parse_ledger.py`.*

## <span class="chip chip-net">IMG</span> Image

The image with **pins**: drop a pin to anchor a comment to a region; react below.
*Example: `Movies.png` — a Jellyfin library banner mock.*

## <span class="chip chip-read">FILE</span> File

Generic, non-previewable blobs. Size, gzip note, **checksum**, and download —
react and discuss even when there's nothing to render. *Example:
`staging-db-dump.sql.gz`, 42.7&nbsp;MB.*

## Bundle

Many files in one share. Humans browse a **tabbed viewer**; agents read the same
files over MCP. Mixed media welcome — created in one shot:

```bash
cairn add audit.md parse_ledger.py Movies.png dump.sql.gz
# ✓ bundle → cairn.sh/9qz1a   ·   4 files · 43.0 MB · ⧗ expires 7d
```

## <span class="chip chip-net">HK</span> Webhook

A **live** requestbin. Point an endpoint (an HTTP ingress URL plus
`mcp://cairn/hook/<id>`) at anything; requests stream into an inspector with
highlighted JSON and a status mix, and to agents over MCP as the same stream.
Requests are **reactable but not comment-threaded** — discussion moves to the
artifacts they produce. See
[ADR-0010](../decisions/ADR-0010.md).

## <span class="chip chip-reason">TRC</span> Trace

**A whole agent run, shared.** The flagship type — a trace is the captured,
shareable record of an agent run.

A **trace** is the artifact; a **run** is the execution it records. That is why
the URL and the tools keep the run noun — `cairn.sh/run/<id>`, `/v1/runs`,
`run_create`/`run_append_spans` — while everything a reader sees says trace. The
stored `share_type` is still `trajectory` and stays accepted on the wire
indefinitely; see [ADR-0016](../decisions/ADR-0016.md).

- A call-trace **waterfall** is pinned up top: OTel-style spans nested by depth, over
  a time ruler, colored by category —
  <span class="chip chip-reason">reason</span>
  <span class="chip chip-exec">exec</span>
  <span class="chip chip-read">read</span>
  <span class="chip chip-net">net</span>
  <span class="chip chip-write">write</span>.
  …and more. Agents label spans either by **operation kind** (`reason · exec · read ·
  net · write · search · plan · tool · analyze · test · fix · fail · meta`) or by
  **workflow phase** (`research · implementation · review · testing · debug · build ·
  docs · delivery · deploy · wait`) — whichever fits how they think. The set is open:
  a category outside both is accepted and gets its own color, derived from its name.
  A **sub-agent** is a nested span group. Click any span to jump to and expand its
  event.
- A readable **activity stream** runs below: the human prompt, each reasoning turn,
  each tool call (expandable args + output), and the `write` that **produces** an
  artifact — linked back as its own share, so a run and its outputs stay connected.
- The right panel carries **provenance** (`claude · sonnet-4.6 · via MCP · captured
  2h ago · expires in 7d`), **run stats** (wall time, spans, tool calls, tokens),
  and a **time-by-category** breakdown.
- React on any turn or tool call; select text or use the `＋` picker to comment.

See [ADR-0009](../decisions/ADR-0009.md)
and [SPEC-0004](../specs/trajectory-share/index.md).

:::note Not in v1
Token-cost lane on the waterfall, run-vs-run diff, and errored-run rendering are
noted as "try next" — deliberately out of the first cut.
:::
