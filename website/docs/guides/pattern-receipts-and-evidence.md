---
title: "Pattern: receipts and evidence"
sidebar_label: Receipts and evidence
sidebar_position: 4.5
description: What goes in Cairn, what goes in your tracker, and what goes in the repo, and how a run's receipt links them.
---

# Pattern: receipts and evidence

When an agent finishes a piece of work, three places could hold the record of it: your
issue tracker, Cairn, and the repository. Put everything in one of them and it either
becomes a junk drawer or loses what it needs. This page sets out which job each one does,
and how to join them with a link.

## The rule

- **The tracker is the status of record.** Who owns the work, whether it's done, and what
  happens next live on the issue. There's one ledger, and it's the tracker.
- **Cairn is the evidence locker, linked from the tracker.** Run receipts, audits, diffs,
  logs, screenshots, and traces go in Cairn, where each gets a short link, provenance, a
  viewer built for its type, comments, and an expiry.
- **The repository holds what must outlive both.** Decision records, specs, and code are
  versioned and reviewed, and they don't expire.

The join is a Cairn link in a tracker comment. The issue says what happened and links the
evidence. The evidence doesn't try to be the status.

## What goes where

| Kind | Where | Why |
|---|---|---|
| Decision records, specs | Repository | Versioned, reviewed, permanent |
| Status, ownership, done-ness | Tracker | One ledger |
| Run receipts, audits, diffs, logs, screenshots, traces | Cairn | Linkable, provenance-stamped, expiring |
| An approval that must survive | Repository | Cairn [expires everything](#retention), for now |

A self-hosting team that plans an agent rollout tends to invent all of this by hand: a
fixed-field receipt, an approval record with a checksum, a link to it from the ticket.
Cairn already covers most of the evidence side. The one gap is keeping something forever,
covered under [Retention](#retention).

## The receipt

A receipt is the note an agent leaves when it finishes: what it did, what changed, and
what a person needs to know. Use the same fixed fields every time, in the same order, so a
reader can scan the answer they need and an agent reading it back can find each one.

| Field | The question it answers |
|---|---|
| Changed | What changed? This is the TL;DR, so it goes first. |
| Affected | Who experiences it? |
| Before | What happened previously? |
| After | What happens now? |
| Evidence | How do we know it's actually active? Links, not assertions. |
| Outcome | Did it work? If that can't be known yet, when will it be measured, and by whom? |
| Human action | What must a person do? The default answer is **Nothing**, and say so. |
| Remaining | What's left, and who owns each item? |
| Learned | What does the system now know that it didn't? "Nothing new" is a valid answer, but write it. |

:::note[Structured receipts aren't shipped yet]
[ADR-0027](../decisions/ADR-0027.md) and [SPEC-0021](../specs/receipts/index.md) design a
structured receipt: a `cairn.receipt/v1` schema, a `receipt_create` tool, and a receipt
card in the viewer. None of that is in Cairn yet. Until it ships, write the receipt as a
markdown artifact using the template below. Its fields are the ones the schema plans to
use, so moving over later is a rename, not a rewrite.
:::

### Markdown template

```markdown
# Receipt: <what was done, in a few words>

**Changed:** <one or two sentences>

**Human action:** Nothing.

## Affected
<who notices the change: a team, a service, its users>

## Before
<what happened previously>

## After
<what happens now>

## Evidence
- [Trace of the run](https://cairn.stump.wtf/run/<run id>)
- [<label>](<link to a log, diff, dashboard, or CI run>)

## Outcome
<measured | pending | not applicable>: <one line>. <If pending: measured on <date> by <whom>.>

## Remaining
- <item> — <owner>

## Learned
<what the system now knows that it didn't, or "Nothing new.">
```

Keep the TL;DR and the human action at the top. Detail belongs behind the evidence links,
not in the receipt.

Create it as a markdown artifact. Over MCP that's `artifact_create` with
`share_type: "markdown"`; see [Creating artifacts and bundles](./connect-your-agent.md#creating-artifacts-and-bundles).
Over REST or the CLI, see [your first share](./first-share.md#create-an-artifact-with-curl).
Tag it `receipt` so you can find every receipt later with `GET /v1/bin?tag=receipt`
([Tags & handoffs](../product/tags.md#setting-tags)). Remember that a tag is a routing
hint the creator asserts, not proof of anything.

## Linking a run to its evidence

Two links tie the work together.

- **The trace points at the receipt.** When an agent captures its run as a trace, a span
  can carry `produced_artifact_id`: the id or `mcp://cairn/<id>` handle of an artifact that
  span produced. The trace viewer shows that span with a link to the artifact, so a reader
  can go from any step of the run to what it made
  ([Trace](../product/share-types.md), [capturing a run](./connect-your-agent.md#capturing-a-run-as-a-trace)).
  The artifact has to exist first: a `produced_artifact_id` that doesn't resolve rejects
  the span batch with `validation_failed`.
- **The tracker points at the receipt.** The agent's last act is a comment on the issue
  with the receipt link. That comment is what makes the evidence findable from the status
  of record.

The receipt can link the trace back through its Evidence list. The easiest order is to
open the run first, which gives you its link straight away, then write the receipt that
links it, then append the span that produced the receipt.

## Integrity

Every body Cairn stores is addressed by its SHA-256. The create response carries it as
`checksum`, over REST and from `artifact_create` alike, and reading the body back with
`GET /v1/artifacts/<id>/body` returns it in the `X-Cairn-Checksum` header. A bundle has no
checksum of its own; reading one of its files returns that file's checksum.

A producer can assert the checksum up front. Send the hex SHA-256 of the body in
`X-Cairn-Sha256` on `POST /v1/artifacts`, and Cairn refuses the upload with
`400 validation_failed` if what arrived hashes to anything else. The MCP create tools and
the `cairn` CLI don't send it, so this is a REST-only check today.

```bash
sum=$(shasum -a 256 receipt.md | cut -d' ' -f1)
curl -sS https://cairn.stump.wtf/v1/artifacts \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -H 'Content-Type: text/markdown' \
  -H "X-Cairn-Sha256: $sum" \
  -H 'X-Cairn-Tags: receipt' \
  --data-binary @receipt.md
```

**What the checksum proves.** The bytes Cairn stored are the bytes the producer hashed,
and nothing truncated or mangled them on the way in. Cairn has no way to edit a body after
it's created, so anyone who downloads it later and gets the same hash is reading exactly
what was written. Record the checksum in the tracker comment and it outlives the artifact:
if the receipt is later copied into the repository, the hash shows it's the same one people
reviewed.

**What it doesn't prove.**

- That the receipt is true. Every field is the creator's claim.
- Who wrote it. That's [provenance](./connect-your-agent.md#what-an-agent-can-and-cant-reach):
  the owner and channel are recorded by the server, and the MCP client name is
  self-reported.
- That anyone approved it. A reaction or a comment isn't a signature.
- That it will still be there. The checksum doesn't stop the artifact expiring.

A SHA-256 isn't a signature either. It catches accidental change, and change by anyone who
can't also rewrite the tracker comment that recorded it.

## Retention

Every Cairn artifact expires ([Expiry](./first-share.md#expiry)):

- **The default is 7 days.** On a self-hosted server it comes from `CAIRN_DEFAULT_TTL`
  ([Configuration](./self-hosting.md#configuration)).
- **The maximum is 30 days.** A longer expiry is rejected, not shortened, and the cap isn't
  configurable.
- **Artifacts an agent creates over MCP always get the default.** The agent can't change
  it. The owner can move the expiry to 1 to 30 days from now in the share dialog
  ([Visibility and access](./first-share.md#visibility-and-access)).
- **An expired artifact is deleted.** Its link and handle return *not found or expired*,
  and nothing brings it back. Rotating a link also breaks every copy of the old one,
  including the one in your tracker.

Opt-in permanent retention is designed in [ADR-0026](../decisions/ADR-0026.md) and
[SPEC-0020](../specs/permanent-retention/index.md), but it isn't shipped. Until it is,
**don't use Cairn as the home of an approval record** or anything else you'll need after
30 days. Keep that in the repository, and let the Cairn copy be the reviewable,
commentable view of it.

This is also why the tracker comment should say more than "see link". Put the one-line
outcome and the human action in the comment itself, so the status of record still reads
correctly after the evidence has expired.

## Worked example

A one-shot agent runs a restore drill against last night's backups, for an issue that
asks for one.

1. **It opens a trace.** `run_create` with `mode: "open"` returns a trace link right away,
   `https://cairn.stump.wtf/run/<run id>`, and the agent appends spans as it works.
2. **It writes the receipt.** When the drill is done, the agent fills in the template,
   with the trace link and the restore log under Evidence, and calls:

   ```text
   artifact_create(
     share_type: "markdown",
     title: "Receipt: nightly backup restore drill",
     body: "# Receipt: nightly backup restore drill\n\n**Changed:** …",
     model: "claude-opus-5",
     tags: ["receipt"]
   )
   ```

   The result carries the receipt's `id`, `url`, `checksum`, and `expires_at`.
3. **It links the run to the receipt.** A final `write` span goes to `run_append_spans`
   with `produced_artifact_id` set to the receipt's id, and the run is closed over REST
   with `POST /v1/runs/<run id>/close`.
4. **It comments on the issue.** The tracker gets one comment:

   ```text
   Restore drill passed: 3 of 3 databases restored and row counts match.
   Human action: nothing.
   Receipt: https://cairn.stump.wtf/<id> (sha256 33894da4…, expires in 7 days)
   Trace: https://cairn.stump.wtf/run/<run id>
   ```

5. **A reviewer picks it up.** They open the receipt from the issue and react 👀 on it, so
   anyone else looking can see it's being read. Then they select the Outcome line and
   comment: "Which restore target? The staging one was rebuilt last week." Both show on
   the receipt, their counts show in the Bin, and a later agent can read the comments with
   `GET /v1/artifacts/<id>/comments` ([Reactions & comments](../product/annotations.md)).
6. **The status moves on the tracker, not in Cairn.** If the answer changes anything, the
   next run writes a new receipt and a new issue comment. When the work is done, the issue
   is closed. The receipt expires on schedule, and the issue still says what happened.
