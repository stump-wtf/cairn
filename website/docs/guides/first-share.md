---
title: Sign in and create your first share
sidebar_label: Your first share
sidebar_position: 2
---

# Sign in and create your first share

This guide takes you from nothing to a working link: sign in, mint a token, push an
artifact, and share it. Budget about five minutes.

## Sign in

1. Open https://cairn.stump.wtf and choose **Sign in with Pocket ID**.
2. Sign in with your single sign-on account. Cairn has no passwords of its own. If you
   don't have an account yet, ask whoever invited you to Cairn.
3. You land on **the Bin**, the list of your artifacts. Each row shows the type,
   provenance, and comment and reaction counts. It stays empty until you create
   something.

The web app is where you read, discuss, and manage what you've shared. It has no upload
button yet, so you create artifacts from a script, the CLI, or your agent.

## Mint a personal access token

A personal access token lets a script, the CLI, or an agent act as you.

1. Open **Settings** (https://cairn.stump.wtf/settings) and go to **API tokens**.
2. Name the token something you'll recognize later, like `laptop scripts`.
3. Pick its scopes:

   | Scope | Allows |
   |---|---|
   | `artifacts:read` | Reading over MCP: `artifact_read`, trace and webhook streams, A2UI views |
   | `artifacts:write` | Creating artifacts, bundles, and traces |
   | `annotations:write` | Commenting and reacting |

4. If an agent will hold the token, tick **This token is for an agent, not a script you
   run yourself**. An agent token can't delete artifacts.
5. Choose **Create token** and copy the token straight away. Cairn shows it exactly
   once. It starts with `cairn_pat_`.

Treat the token like a password. You can revoke it from the same list at any time.

## Create an artifact with curl

Every surface talks to the same REST API, so `curl` is the quickest way to create
something with no extra tooling:

```bash
export CAIRN_TOKEN='cairn_pat_…'   # load it from your password manager

curl -sS https://cairn.stump.wtf/v1/artifacts \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -H 'Content-Type: text/markdown' \
  -H 'X-Cairn-Title: incident notes' \
  --data-binary @notes.md
```

Cairn answers `201 Created` with the new artifact (trimmed here):

```json
{
  "id": "<id>",
  "url": "https://cairn.stump.wtf/<id>",
  "mcp": "mcp://cairn/<id>",
  "share_type": "markdown",
  "title": "incident notes",
  "media_type": "text/markdown",
  "visibility": "link",
  "provenance": {"actor": "you@example.com", "channel": "via API", "captured_at": "…"},
  "expires_at": "…"
}
```

Open the `url` in a browser. That's your first share.

These request headers are worth knowing:

| Header | Effect |
|---|---|
| `Content-Type` | The media type, which picks the viewer: `text/markdown` renders as a document, `text/x-python` highlights as code, `image/png` shows an image |
| `X-Cairn-Title` | The title shown in the Bin and the page header |
| `X-Cairn-Ttl-Seconds` | Expiry in seconds, up to 30 days (`2592000`); leave it out for the 7-day default |
| `X-Cairn-Type` | Force a share type (`markdown`, `code`, `image`, `file`); rarely needed |
| `X-Cairn-Model` | The model that produced the content, shown as provenance |
| `X-Cairn-Tags` | Comma-separated routing tags, such as `handoff,lane:m`; see [Tags & handoffs](../product/tags.md) |

To push several files as one bundle, send a multipart form. Every part with a filename
becomes a file in the bundle, and a plain `title` field names it:

```bash
curl -sS https://cairn.stump.wtf/v1/artifacts \
  -H "Authorization: Bearer $CAIRN_TOKEN" \
  -F title='backup audit' \
  -F 'file=@audit.md;type=text/markdown' \
  -F 'file=@fix.patch' \
  -F 'file=@disk-usage.png'
```

## Or use the cairn CLI

The `cairn` CLI wraps the same API: pipe something in and you get a link back, copied to
your clipboard.

:::note[Not publicly distributed yet]
There's no public download, Homebrew formula, or `go install` path for the CLI yet. Until
there is, use `curl` or your agent. If someone has given you a build, the commands below
work against the hosted service.
:::

The CLI's built-in default server isn't the hosted service, so point it at Cairn first:

```bash
export CAIRN_URL=https://cairn.stump.wtf

cairn login          # paste your token at the hidden prompt
cairn whoami

cat notes.md | cairn                        # one artifact from stdin
cairn report.md                             # one artifact from a file
cairn add audit.md fix.patch disk-usage.png # several files, one bundle
cairn --ttl 24h --title "incident notes" incident.md
cat prompt.md | cairn --tag handoff --tag lane:m   # tagged for a handoff
```

The flags you'll use most are `--ttl` (`30m`, `24h`, `7d`; the server rejects anything
over 30 days), `--title`, `--tag` (repeat it, or pass a comma-separated list), `--type`
(overrides the detected media type), `--json` for machine-readable output, and
`--no-copy` to leave your clipboard alone. The CLI keeps your
token in the OS keychain when it can, and otherwise in a `0600` file in your config
directory. `cairn logout` deletes only that local copy. To kill the token itself, revoke
it in Settings.

## Expiry

Every artifact expires. The default is 7 days after creation, and you can ask for
anything up to 30 days when you create it. Once it expires, the web link and the MCP
handle both return *not found or expired*, and Cairn deletes the content. There's no
recycle bin.

Artifacts your agent creates over MCP always get the default expiry. To keep one longer,
change it from the web (next section).

## Visibility and access

The **Share** button in an artifact's header opens the share dialog. Anyone who can see
the artifact can copy its **web link** and **MCP handle** there, and see its access,
expiry, and provenance. If you own the artifact and you're signed in, the dialog also
has three controls:

- **Extend / shorten expiry** sets the expiry to 1 to 30 days from now.
- **Link access** switches between *you + anyone with link* (the default) and *you only*.
- **Rotate link** mints a new id. The old link and the old MCP handle stop working
  immediately, for everyone, and you can't undo it.

These controls are web-only. Tokens and MCP agents can't change sharing, expiry, or the
id.

:::warning["You only" doesn't lock the link yet]
Right now *you only* changes how the artifact is labeled, but anyone who already has the
link can still open it on the web, over the API, or over MCP. If a link has gone
somewhere it shouldn't, **Rotate link** is what actually cuts off access. Until this is
fixed, assume anyone holding a link can read the artifact.
:::

There's no delete button on the web yet. To delete an artifact you own before it
expires, use a token that isn't marked as an agent token and has `artifacts:write`:

```bash
curl -sS -X DELETE "https://cairn.stump.wtf/v1/artifacts/<id>" \
  -H "Authorization: Bearer $CAIRN_TOKEN"
```

## Comments and reactions

Anyone with the link can read an artifact's comments and reactions. Sign in to add your
own.

- **React** with an emoji to the whole artifact or to part of it: a markdown block or
  bullet, a code line, an image region, a trace span, or one captured webhook request.
- **Comment** on a text selection, a code line or range, an image pin, a trace span, or
  the whole artifact. Replies go one level deep.
- Captured webhook requests take reactions but not comments.

Agents do the same over MCP with `artifact_comment` and `artifact_react`. For every
anchor type, see [Reactions & comments](../product/annotations.md).

## Links and handles

Every artifact has two addresses, and both carry the same id.

| Address | Looks like | Used by |
|---|---|---|
| Web link | `https://cairn.stump.wtf/<id>`, or `https://cairn.stump.wtf/run/<id>` for a trace | People, in a browser |
| MCP handle | `mcp://cairn/<id>`, `mcp://cairn/run/<id>` for a trace, `mcp://cairn/hook/<id>` for a webhook | Agents, through Cairn's MCP server |

A browser can't open an MCP handle. Give it to an agent that has Cairn connected, and the
agent passes it (or just the bare id) to `artifact_read`. Rotating a link breaks both
addresses.

Next, [connect your agent](./connect-your-agent.md).
