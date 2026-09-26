---
title: Annotations
sidebar_position: 3
slug: /annotations
---

# Reactions & comments

One annotation subsystem serves every share type, with **type-specific anchors**
(see [ADR-0006](../decisions/ADR-0006.md)
and [SPEC-0006](../specs/annotations/index.md)).

## Reactions

Emoji reactions (🔥 🙏 🎉 👀 …) with a `＋` picker, anchorable to almost anything —
a markdown block or bullet, a code line or selection, an image region, a webhook
request, a trace turn / tool call / span, or the whole artifact.

## Comments

Threaded comments, anchored to a text selection (they land in the right margin /
panel), a code line, an image-region pin, or the whole artifact.

## Anchors by type

| Share type | Reaction anchors | Comment anchors |
|------------|------------------|-----------------|
| Markdown | block, bullet, whole | text selection, whole |
| Code | line, selection, whole | line, selection, whole |
| Image | region pin, whole | region pin, whole |
| File | whole | whole |
| Webhook | **single request**, whole | — *(reactions only)* |
| Trace | turn, tool call, span, whole | span, text selection, whole |

:::note[The webhook exception]
Webhook requests are **reactable but not comment-threaded** — a shared triage signal,
by design. *"Discussion happens on the artifacts they produce, not here."*
:::

## How it's modeled

Anchors are polymorphic — `{artifact_id, anchor_type, anchor_ref}`, where
`anchor_ref` is a type-specific locator (a line range, a block id, a region, a span
id, a request id, or character offsets). Reactions are idempotent per identity +
anchor; counts are aggregated and surfaced in the Bin (`💬 2 · 👀 3`) and in headers
(`6 comments · 16 reactions`).
