---
title: Overview
sidebar_label: Overview
sidebar_position: 1
---

# Cairn

**Cairn is an AI-native artifact-sharing service** — a pastebin, gist, and
requestbin reimagined for the agent era. Humans and agents both produce ephemeral,
shareable artifacts; Cairn gives every one a short URL, a viewer built for its type,
provenance, reactions, and comments — and makes the *same* artifact readable and
writable by agents over MCP.

> *A cairn is a trail marker — a small stack of stones left to guide whoever comes
> next. That's the shape of the product: **what an agent has dropped for you**, and
> the human hands it onward — to people and to agents.*

```bash
cat checkout-web-audit.md | cairn      # → cairn.sh/9qz1a  (copied)
```

## The core loop

**index → read / react / comment → share**, symmetric across humans (web + CLI) and
agents (MCP):

- **Human poster** pipes shell output (`cat file | cairn`), adds files, or lands
  artifacts via their agent — and gets a link to share.
- **Agent** (Claude et al.), authorized over MCP OAuth, reads, creates, comments,
  and reacts on the human's behalf — dropping "receipts" for the human to review.
- **Human reader** opens the link, reads, reacts, comments, and hands it onward.

## Core concepts

- **Artifact** — a single shared thing: an id, a **share type**, a body, metadata,
  provenance, an access policy, a TTL, and an annotation stream. Addressed by a
  short URL.
- **Share type** — the *kind* of artifact, which drives its viewer, its metadata
  panel, and its annotation affordances. The type set is **extensible**.
- **Bundle** — one artifact containing many files (mixed media), browsed in a tabbed
  viewer; agents read the same files over MCP.
- **Workspace** — a person's space of artifacts, with people and access.
- **The Bin** — the listing/index of artifacts, in both web and TUI.

## Share types at a glance

| Type | Badge | What it is |
|------|-------|------------|
| Markdown | <span class="chip chip-read">MD</span> | Rendered doc + TOC; react on blocks/bullets, select-to-comment. |
| Code | <span class="chip chip-exec">PY</span> | Syntax highlight, line numbers, symbol outline; comment a line/selection. |
| Image | <span class="chip chip-net">IMG</span> | Pins anchor region comments; react below. |
| File | <span class="chip chip-read">FILE</span> | Non-previewable blobs: size, checksum, download, discuss. |
| Bundle | — | Many files, one link — a tabbed viewer; agents read them over MCP. |
| Webhook | <span class="chip chip-net">HK</span> | A live requestbin: requests stream in, reactable, readable over MCP. |
| Run | <span class="chip chip-reason">RUN</span> | A whole agent run — an OTel-style span waterfall + activity stream. |

See **[Share types](./product/share-types.md)** for the full tour.

## Three surfaces, one core

Every surface rides the same app shell and the same core service:

- **Web** — one shell for every type: logo · type · one URL control · share ·
  collapsible metadata + comments panel. Plus **the Bin**.
- **CLI** (`cairn`) — *pbcopy for cairn*: pipe or add files, browse the Bin in a
  keyboard-driven TUI.
- **MCP** — agents read, create, comment, and react over MCP, authorized via OAuth.

See **[Surfaces](./product/surfaces.md)**.

## How this site is organized

This documentation is generated **spec-first**. The design is captured as
[Architecture Decision Records](./decisions/index.mdx) (the *why*) and
[OpenSpec specifications](./specs/index.mdx) (the *what*), and both render here in
full — index rows, status badges, requirement counts, and cross-references are
computed from those files at build time rather than written by hand.

:::tip
There is no "view source" link to follow. The whole record is on this site.
:::
