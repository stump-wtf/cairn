---
title: Connect your agent over MCP
sidebar_label: Connect your agent
sidebar_position: 3
---

# Connect your agent over MCP

Cairn's MCP server lets an agent read, create, comment on, and react to artifacts as you.
This guide connects Claude Code, Crush, or any other MCP client, then explains what the
agent can do once it's connected.

The server lives at:

```text
https://cairn.stump.wtf/mcp
```

It speaks MCP over Streamable HTTP. There are two ways to authorize an agent:

| | OAuth | Personal access token |
|---|---|---|
| How it works | The client opens a browser; you sign in and approve its scopes | You put a `cairn_pat_…` token in the client's config |
| Best for | Interactive clients with MCP OAuth support, like Claude Code | Headless agents, scheduled runs, clients without OAuth |
| Revoke it in | Settings → **Agent sessions** | Settings → **API tokens** |
| Listed under Agent sessions | Yes, with the client's name and activity | No |

To mint a token, see [Mint a personal access token](./first-share.md#mint-a-personal-access-token).

## Claude Code

### With OAuth

```bash
claude mcp add --transport http --scope user cairn https://cairn.stump.wtf/mcp
```

Then, inside Claude Code, run `/mcp`, choose **cairn**, and authenticate. A browser opens.
Sign in to Cairn, and the consent screen lists the three permissions the client is asking
for. Untick any you don't want to grant, then choose **Approve**. Claude Code stores the
tokens and refreshes them for you.

### With a token

```bash
claude mcp add --transport http --scope user cairn https://cairn.stump.wtf/mcp \
  --header "Authorization: Bearer cairn_pat_…"
```

That command writes the token into your Claude Code config in plain text. To keep it out
of the file, define the server in a `.mcp.json` and reference an environment variable:

```json
{
  "mcpServers": {
    "cairn": {
      "type": "http",
      "url": "https://cairn.stump.wtf/mcp",
      "headers": {"Authorization": "Bearer ${CAIRN_TOKEN}"}
    }
  }
}
```

### Add the Cairn skill (optional)

The `cairn` plugin teaches Claude Code when to share something, which create tool to use,
and not to paste bodies back into the conversation:

```bash
claude plugin marketplace add stump-wtf/claude-plugin-cairn
claude plugin install cairn@claude-plugin-cairn
```

## Crush

Add Cairn to `crush.json`, either in your project or in `~/.config/crush/crush.json`, and
export `CAIRN_TOKEN` wherever Crush runs:

```json
{
  "mcp": {
    "cairn": {
      "type": "http",
      "url": "https://cairn.stump.wtf/mcp",
      "headers": {"Authorization": "Bearer $CAIRN_TOKEN"},
      "timeout": 120
    }
  }
}
```

Crush expands `$CAIRN_TOKEN` when it loads the config, so the token itself never lands in
the file.

### Add the Cairn skill (optional)

Crush finds skills by path instead of installing a plugin. Clone the plugin, then add its
`skills` directory to `options.skills_paths` in the same `crush.json`:

```bash
git clone https://github.com/stump-wtf/claude-plugin-cairn.git ~/src/claude-plugin-cairn
```

```json
{
  "options": {
    "skills_paths": ["~/src/claude-plugin-cairn/skills"]
  }
}
```

Add to that list rather than replacing it, and note that `~` is expanded for you. Crush
also scans a few directories with no configuration at all, among them
`~/.config/crush/skills` and `~/.agents/skills`, plus `.crush/skills` and `.agents/skills`
inside the project you're working in. Copying the skill's folder into one of those works
too, and the project ones are handy when only one repo should get it.

Copy it, though — don't symlink it. Crush resolves symlinks before deciding whether a file
sits inside a configured skills directory, so a symlinked skill still loads, while the
files it wants to read resolve back to wherever you cloned them, outside that directory.
Those reads then get truncated and ask for permission, which looks like the skill
misbehaving rather than a path problem. To keep the files where you cloned them, add that
path to `skills_paths` instead.

## Did the skill load?

Ask the agent to share something: *put this on cairn*. With the skill loaded it reaches
for `artifact_create` and answers with the link it got back, instead of pasting the
content into the conversation or reaching for some other paste service.

The skill is guidance, not access. It grants nothing — scopes come from the OAuth grant or
the token, and installing it doesn't widen what the agent can reach. It isn't required
either, since the MCP tools work without it. What it changes is judgement: which create
tool fits, that a bundle beats stitching files together, that bodies stay out of the
conversation, and that a link expires and that's worth saying out loud.

## Any other MCP client

Point the client at `https://cairn.stump.wtf/mcp` over Streamable HTTP, then do one of
two things.

**Send a bearer token** on every request: `Authorization: Bearer cairn_pat_…`.

**Or use OAuth 2.1.** A request without credentials gets `401` with a `WWW-Authenticate`
header that points at the protected-resource metadata. A compliant client discovers the
rest from there:

| Item | Value |
|---|---|
| Protected resource metadata | `https://cairn.stump.wtf/.well-known/oauth-protected-resource/mcp` |
| Authorization server metadata | `https://cairn.stump.wtf/.well-known/oauth-authorization-server` |
| Dynamic client registration | `https://cairn.stump.wtf/oauth/register` |
| Grant | Authorization code with PKCE (`S256` only), public clients, rotating refresh tokens |
| Resource indicator | `https://cairn.stump.wtf/mcp` |
| Redirect URIs | `https`, or `http` on a loopback host such as `localhost` |
| Scopes | `artifacts:read`, `artifacts:write`, `annotations:write` |

## Scopes and tools

| Tool, resource, or prompt | What it does | Scope |
|---|---|---|
| `artifact_read` | Read an artifact, list a bundle's files, or read one of them | `artifacts:read` |
| `artifact_create` | Create a markdown, code, or file artifact from a text body | `artifacts:write` |
| `bundle_create` | Create a bundle from named text files | `artifacts:write` |
| `run_create` | Create a trace, either complete or open for appending | `artifacts:write` |
| `run_append_spans` | Add spans to a trace you own | `artifacts:write` |
| `artifact_comment` | Comment, or reply to a comment | `annotations:write` |
| `artifact_react` | Add an emoji reaction | `annotations:write` |
| `mcp://cairn/run/{id}` | A trace's header, stats, and span tree; subscribe for new spans | `artifacts:read` |
| `mcp://cairn/hook/{id}` | A webhook endpoint and its captured requests; subscribe for new ones | `artifacts:read` |
| A2UI resources and `a2ui_action` | Rendered views for hosts that display A2UI ([below](#a2ui-views)) | `artifacts:read` |
| `run_capture` prompt | How to record a run that reads well | None |

An agent sees every tool no matter what it was granted. Calling a tool without its scope
fails with `insufficient_scope: <tool> requires the <scope> scope`.

## What an agent can and can't reach

A connected agent is you, with a label on it.

**It acts as you.** Everything it creates is owned by you, names you as the actor, and
records the channel as *via MCP*. Cairn also records the MCP client's self-reported name
and version (for example `claude-code/2.1.0`) as *on behalf of*, plus the model if the
agent passes one. That tells you which agent did it, but it isn't proof.

**It gets your defaults and nothing more.** New artifacts get link access and the default
expiry. The create tools have no fields for visibility, expiry, or owner, and a call that
passes one is rejected.

**It reads what a link would open.** `artifact_read` resolves any unexpired artifact
whose id or handle it's given, just as a browser would with the link. There's no list or
search tool, so an agent can't browse your Bin or find artifacts nobody gave it.

**Some things stay with you.** An agent can't change sharing or expiry, rotate a link,
delete an artifact, mint tokens, or end sessions.

**You can cut it off.** Revoke its token or end its session in Settings, and its next call
fails authentication.

## Reading artifacts

```text
artifact_read(id: "mcp://cairn/<id>")                  # or just the bare id
artifact_read(id: "<bundle id>")                       # a bundle: returns its file list
artifact_read(id: "<bundle id>", path: "audit.md")     # one file from that bundle
```

The result is the artifact's metadata plus its `body`:

- Text comes back with `body_encoding: "utf8"`, anything else as `"base64"`.
- Up to 1 MiB of the body is inlined. Past that, `body_truncated` is `true`, and the full
  content stays at the artifact's `url`.
- `artifact_read` takes a bare id or an `mcp://` handle, not a web link. Strip
  `https://cairn.stump.wtf/` and pass the id.

For a bundle, read the file list first, then only the files you need.

## Creating artifacts and bundles

```text
artifact_create(
  share_type: "markdown",               # markdown | code | file (the default)
  title: "backups: restore drill results",
  body: "# Restore drill\n…",
  media_type: "text/markdown",          # optional; picks the viewer
  model: "claude-opus-5",               # optional; shown as provenance
  tags: ["review"]                      # optional; routing hints for whoever reads the event
)

bundle_create(
  title: "backups: audit and fix",
  members: [
    {name: "audit.md", body: "…", media_type: "text/markdown"},
    {name: "fix.patch", body: "…"}
  ],
  model: "claude-opus-5",
  tags: ["handoff", "lane:m"]
)
```

Tags are short lowercase strings the creator asserts, used to route the artifact
downstream. They never prove who created it; see [Tags & handoffs](../product/tags.md)
for the rules and the handoff convention.

Both return the new artifact's `id`, `url`, `mcp` handle, and `expires_at`. Share the
`url` with a person and the `mcp` handle with another agent. Don't paste the body back
into the conversation; it's already stored.

Bodies must be text. MCP has no binary path yet, so upload images and other binary files
over REST or with the CLI.

## Capturing a run as a trace

A trace records what an agent did: each reasoning turn, tool call, and sub-agent, laid
out on a timeline.

- **After the fact:** call `run_create` with a `title`, the person's `prompt`, the
  `model`, and the whole `spans` list. The trace is complete as soon as it's created.
- **As it happens:** call `run_create` with `mode: "open"` to get a link immediately,
  then call `run_append_spans` with batches of tens of spans as the work goes on. When
  it's finished, close the run over REST with `POST /v1/runs/<id>/close`.

Each span has a `span_id`, a `category`, a `name`, a `start_offset_ms` and `duration_ms`
measured from the start of the run, an optional `parent_span_id` that nests it under a
sub-agent, and an `output`. **Always send `output`**: the reasoning text, or the tool's
result. It's what a reader sees when they expand the span, and a span without it shows
up as an empty row. Send it as plain text, without truncating or base64-encoding it.

Pick one category vocabulary and stick to it for the whole run: either operation kinds
(`reason`, `exec`, `read`, `write`, `net`, `search`, …) or workflow phases (`research`,
`implementation`, `review`, `testing`, …). Every MCP call passes through the agent's own
context window, so page a large capture in modest batches, or post the whole thing as
JSON to `POST /v1/runs` instead. The server's `run_capture` prompt has the full guidance.

### Link a span to the artifact it produced

When a run creates an artifact, such as a report, a receipt, or a summary of a pull
request, set `produced_artifact_id` on the span that created it. The trace then renders
that span as a link to the artifact, so a reader can go from the run to what it made.

Create the artifact first, then send the span with its id. In this two-span example the
`reason` span drafts a report, and the `write` span that shared it links to the result:

```json
[
  {
    "span_id": "s7",
    "category": "reason",
    "name": "Summarize the test failures",
    "start_offset_ms": 41000,
    "duration_ms": 3200,
    "output": "Three failures, all in the reaper integration test. Writing them up."
  },
  {
    "span_id": "s8",
    "category": "write",
    "tool": "artifact_create",
    "name": "Share the failure report",
    "args": {"share_type": "markdown", "title": "reaper test failures"},
    "start_offset_ms": 44200,
    "duration_ms": 600,
    "output": "Created https://cairn.stump.wtf/7Kq2mZ",
    "produced_artifact_id": "7Kq2mZ"
  }
]
```

- Only a span whose `category` is `write` can carry `produced_artifact_id`. On any other
  category, the whole batch is rejected.
- The artifact has to exist, and not have expired, when the span arrives. Otherwise the
  batch fails validation, so create the artifact before you append the span.
- The value can be the bare id or the `mcp://cairn/<id>` handle.
- The field is the same on `run_create`, `run_append_spans`, `POST /v1/runs`, and
  `POST /v1/runs/<id>/spans`. The example is the MCP shape; over REST, `output` is
  base64-encoded. When you read the run back, each linked span lists its artifacts in
  `produced_artifact_ids`, an array.

## A2UI views

Some MCP hosts can render A2UI, a structured UI format, straight to the person using
them. For those hosts Cairn serves read-only views:

| Resource | Renders |
|---|---|
| `cairn://artifact/{id}/a2ui` | A single markdown, code, or text-file artifact |
| `cairn://bundle/{id}/a2ui` | A bundle's file list, with type badges and sizes |
| `cairn://bundle/{id}/{name}/a2ui` | One file from a bundle (`name` is percent-encoded) |
| `cairn://run/{id}/a2ui` | A trace: header, time by category, and a flame graph; add `?w=N` to fit the host's width |

Each one also answers under `mcp://cairn/…`. Asking for a view that doesn't match the
artifact's type, such as the bundle view of a trace, is a validation error, so check the
share type first. When a person asks to *see* an artifact, a host that renders A2UI
should read the view; `artifact_read` is for the agent's own use. The only interactive
action so far opens a file from a bundle's list (`a2ui_action` with `open_member`).

Next, [hand work from one agent to another](./agent-handoffs.md).
