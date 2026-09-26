---
title: What Cairn is for
sidebar_label: What Cairn is for
sidebar_position: 1
---

# What Cairn is for

Cairn is a place to put things you want someone else to look at: a report, a diff, a
screenshot, a log, a whole agent run. You or your agent push it in, and Cairn gives it a
short link. Whoever opens the link gets a viewer built for that kind of thing, plus
comments and reactions. Agents read and write the same artifacts over MCP, so the link
you paste into a chat and the handle you give another agent point at the same object.

The hosted service is at https://cairn.stump.wtf.

## Reach for Cairn when…

| Situation | Why a Cairn link beats pasting it into chat |
|---|---|
| The output is long: an audit, a migration plan, a 400-line log | Chat mangles it and scrolls it away. A link stays readable and you can send it again. |
| Several files belong together | A bundle keeps them behind one link with a file rail, and agents can read each file by name. |
| You want feedback on a specific line, paragraph, or region | Comments and reactions anchor to a code line, a markdown block, an image pin, or a trace span. |
| Another agent should pick the work up | Give it an `mcp://cairn/<id>` handle and it reads the artifact with `artifact_read`. See [Agent handoffs](./agent-handoffs.md). |
| You want to show what an agent actually did | A trace captures the run as a timeline plus an activity stream. |
| You're debugging a webhook | A webhook endpoint captures every request sent to it. See [Webhook inspector](./webhook-inspector.md). |

## Don't use it for…

- **Secrets.** Anyone who has a link can read the artifact behind it (see
  [Visibility and access](./first-share.md#visibility-and-access)). Never put a token,
  password, or key in a body or a title.
- **Permanent storage.** Artifacts expire: 7 days by default, 30 days at most. Cairn is
  for sharing, not archiving.
- **A one-liner.** If it fits in a sentence, send the sentence.

## Share types

Every artifact has a share type. It decides the viewer, the side panel, and what you can
anchor a comment to.

| Share type | What it is | How you usually create it |
|---|---|---|
| Markdown (`markdown`) | A rendered document with a table of contents | `artifact_create` with `share_type: "markdown"`, or upload a `text/markdown` body |
| Code (`code`) | Highlighted source with line numbers and a symbol outline | Upload a source file, or `artifact_create` with `share_type: "code"` |
| Image (`image`) | The image, with pins that anchor comments to a region | Upload the file over REST or the CLI (MCP can't carry binary bodies yet) |
| File (`file`) | Anything Cairn can't preview: size, checksum, and a download | Upload any file; Cairn falls back to this when nothing richer fits |
| Bundle (`bundle`) | Many files behind one link | `bundle_create`, `cairn add`, or a multipart upload |
| Webhook (`webhook`) | A live requestbin that captures whatever is sent to it | `POST /v1/hooks`; see [Webhook inspector](./webhook-inspector.md) |
| Trace (`trajectory`) | A whole agent run: a span timeline on top, an activity stream below | `run_create` and `run_append_spans`, or `POST /v1/runs` |

You rarely need to choose the type for an upload. When the media type says markdown, an
image, or source code, Cairn promotes a plain file upload to the richer type. For the full
tour, see [Share types](../product/share-types.md).

:::note[Trace, not trajectory]
Everything a person reads says *trace*. The stored `share_type` is still `trajectory`,
and the URLs and tools still say *run* (`/run/<id>`, `run_create`).
:::

## From zero to one

1. [Sign in and create your first share](./first-share.md): the web app, a token, and one
   `curl` command.
2. [Connect your agent over MCP](./connect-your-agent.md): Claude Code, Crush, or any MCP
   client.
3. [Hand work from one agent to another](./agent-handoffs.md): the handoff pattern.
4. [How Harness, Switchboard and Cairn fit together](./how-it-fits.md): when you want
   handoffs to run on their own.

If something goes wrong, see [Troubleshooting](./troubleshooting.md).
