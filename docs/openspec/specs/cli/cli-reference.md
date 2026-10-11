# Cairn CLI Reference

Every `cairn` command, flag and exit code. The requirements behind them are in
[SPEC-0008](spec.md); this page describes what the binary does today. It was
checked against `cairn --help` and each subcommand's `--help`, the exit codes in
`internal/cliexit/exit.go`, and the configuration resolver in
`internal/cliconfig/config.go`. Where they disagree with this page, the code wins
and this page is stale.

## Commands

| Command | What it does |
|---------|--------------|
| `cairn [file]` | Create one artifact from stdin or from a single file path |
| `cairn add <file>...` | Push several files as one bundle artifact |
| `cairn read <id\|handle>` | Print an artifact's body, or a bundle's file list |
| `cairn comment <id\|handle> [body]` | Comment on an artifact |
| `cairn login` | Verify a bearer token against the server and store it |
| `cairn whoami` | Show the current authentication state |
| `cairn logout` | Remove this machine's stored credential |
| `cairn serve` | Run the Cairn server (web app, `/v1` API, SSE, MCP) in this process |
| `cairn completion <shell>` | Print a completion script for `bash`, `zsh`, `fish` or `powershell` |
| `cairn help [command]` | Help for any command |

### `cairn [file]`

Reads stdin, or the one file named on the command line, and creates a single
artifact. It prints the link on stdout and, on an interactive terminal, copies it
to the clipboard.

```bash
cat incident.md | cairn
cairn report.pdf
cairn --ttl 24h --title "meeting notes" notes.md
cat report.md | cairn --json
```

It accepts at most one file. Passing several is a usage error (exit `2`); use
`cairn add` for that.

Piped or non-interactive output is only the bare link, so it composes in scripts:

```bash
url=$(cat report.md | cairn)
```

### `cairn add <file>...`

Pushes several files as one bundle artifact. `--concurrency` bounds the worker
pool that prepares the files locally (default `4`).

```bash
cairn add screenshot.png error.log database.sql
cairn add --title "incident dump" --ttl 7d screenshot.png error.log
```

### `cairn read <id|handle>`

Prints one artifact's body to stdout. The one argument is a bare id or the
`mcp://cairn/<id>` handle — the same handle a handoff carries — so an agent can
open its work order without stripping a prefix first.

```bash
cairn read 9qz1a
cat report.md | grep -c TODO   # …or pipe the body somewhere: cairn read 9qz1a | pbcopy
cairn read mcp://cairn/9qz1a --path notes.md
cairn read 9qz1a --json
```

A bundle has no body of its own, so reading one without `--path` prints its file
list (name, size, media type) and its link instead of dumping every member; the
listing is what tells you what `--path` can take.

```bash
cairn read bnd1
4 files · 1.2 MB · ⧗ expires 6d · 🔒 you + anyone with link
  notes.md    8.1 kB  text/markdown
  run.log     1.2 MB  text/plain
  https://cairn.stump.rocks/bnd1
```

`--json` prints one metadata object — share type, media type, size, tags,
provenance, expiry, members — with the body inlined up to 1 MiB and
`body_truncated: true` when only a prefix fit. The whole body is always at
`url`; the ceiling exists because a JSON body is read by something that holds it
in memory. Without `--json` the body is streamed in full, undecorated, so piping
composes. Reading needs no credential: an artifact's link is the capability
(`cairn whoami` may say you are not logged in and `cairn read` will still work).

### `cairn comment <id|handle> [body]`

Posts one comment on an artifact and prints its id and the artifact's link. The
body is the argument, a file, or stdin — whichever avoids quoting it:

```bash
cairn comment 9qz1a "looks good, one nit below"
cairn comment mcp://cairn/9qz1a --file reply.md
cat reply.md | cairn comment 9qz1a
cairn comment 9qz1a --parent 41 "replying to that"
cairn comment 9qz1a "hi" --json
```

It requires a session (exit `3` without one, with no request sent) and refuses an
empty body (exit `2`). The comment is anchored to the whole artifact; the server
decides whether the share type accepts comments at all, and says so in the
violation the CLI prints. A bare `cairn comment <id>` at a terminal is a usage
error rather than a read that never ends.

### `cairn login`

Verifies a bearer token with a whoami round trip, then stores it for later
commands. Mint a personal access token from the Cairn web Settings page, or use a
token your deployment operator issued:

```bash
cairn login --token <token>     # pass it directly (never echoed, never logged)
echo "$TOKEN" | cairn login     # pipe it in, e.g. from a secrets manager
cairn login                     # interactive prompt with input hidden
```

This is a token login. The browser OAuth 2.1 authorization-code + PKCE flow that
MCP clients use (SPEC-0007) is not available in the CLI yet.

### `cairn whoami`

Shows who the credential authenticates as, where the token came from, and which
server it was checked against:

```
✓ authorized as <actor> · <channel>
token source: <flag|env|file|keyring> · server: <api base url>
```

### `cairn logout`

Deletes the locally stored token from the OS keyring and/or the `0600` config
file, wherever `cairn login` put it. It does **not** revoke the token on the
server or remove any other machine's copy; revoke it from the web Settings page
for that.

### `cairn serve`

Runs the web app, the `/v1` REST/JSON API, the SSE streams and the MCP server in
this process. It is the former `cairnd` binary, folded into `cairn` as a
subcommand ([ADR-0031](../../../adrs/ADR-0031-one-cairn-binary-the-server-is-cairn-serve.md)).

`serve` takes no flags of its own. Its whole configuration is the `CAIRN_*`
environment — the same variables, with the same meanings, that `cairnd` read —
for example `CAIRN_HTTP_ADDR`, `CAIRN_DATABASE_URL`, `CAIRN_BASE_URL` and the
`CAIRN_S3_*` storage settings. The
[self-hosting guide](https://cairn.stump.wtf/docs/guides/self-hosting/) lists every
variable and walks through a full deployment. SIGINT and SIGTERM shut the server
down gracefully.

Upgrading from `cairnd` changes only the command name. The container image
`ghcr.io/stump-wtf/cairn` still starts through a `cairnd` shim that runs
`cairn serve`, so container deployments need no change.

## Flags

Every command accepts these flags. Most of them matter only to the commands that
create artifacts.

| Flag | Description |
|------|-------------|
| `--url <url>` | API base URL (env `CAIRN_URL`, default `https://cairn.stump.wtf`) |
| `--token <token>` | Bearer token (env `CAIRN_TOKEN`) |
| `--json` | Emit machine-readable JSON instead of human-readable output |
| `-v`, `--verbose` | Structured diagnostic output on stderr, with tokens redacted |
| `--title <title>` | Display title for the artifact or bundle |
| `--ttl <duration>` | Request an expiry, e.g. `24h` or `7d`; the server decides whether to honor it |
| `--tag <tag>` | Attach a routing tag; repeatable or comma-separated (`--tag handoff --tag lane:auto`) |
| `--type <media-type>` | Override the detected Content-Type |
| `--redact mask` | Store detected credentials as `[REDACTED]` instead of refusing the upload. `mask` is the only value |
| `--no-copy` | Never copy the resulting link to the clipboard |
| `--concurrency <n>` | Worker pool size for `cairn add`'s local file preparation (default `4`) |
| `--path <name>` | `cairn read`: stream one bundle member instead of listing the bundle's files |
| `--file <path>` | `cairn comment`: read the comment body from a file instead of the argument or stdin |
| `--parent <comment-id>` | `cairn comment`: post a reply to that comment, inheriting its anchor |
| `--version` | Print the version |
| `-h`, `--help` | Help for the command |

## Configuration and credentials

The API URL and the token each resolve in this order; the first match wins:

1. the flag (`--url`, `--token`);
2. the environment (`CAIRN_URL`, `CAIRN_TOKEN`);
3. the config file `cairn/config.toml` under the OS config directory
   (`$XDG_CONFIG_HOME`, else `~/.config`, on Linux; `~/Library/Application Support`
   on macOS), or the token `cairn login` stored;
4. the default URL, `https://cairn.stump.wtf`.

A malformed config file or an invalid URL is a usage error (exit `2`), reported
before any network call.

`cairn login` stores the token in the OS keyring when one is reachable (macOS
Keychain, Secret Service/libsecret on Linux), and otherwise in a `0600` file
under the config directory. Tokens are never logged or printed.

## Exit codes

Scripts can branch on `$?`. The values are stable and are never renumbered.

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Internal or unclassified error |
| `2` | Usage error: bad flags or arguments, empty input, or a server `validation_failed` |
| `3` | Not authenticated, or the credential was rejected |
| `4` | Forbidden |
| `5` | Not found, or expired |
| `6` | Payload too large |
| `7` | Rate limited |
| `8` | Network error: DNS, TLS or connection |
| `9` | Conflict |
| `130` | Interrupted (Ctrl-C) |

## Troubleshooting

- **Exit `3` on every command.** No usable token. Run `cairn whoami` to see what
  resolved, then `cairn login` again. Check that `CAIRN_TOKEN` is not set to a
  stale value: the environment beats the stored token.
- **Exit `8`.** The CLI could not reach the server. Check `--url` / `CAIRN_URL`,
  and that the host resolves and serves TLS.
- **Exit `7`.** The server is rate-limiting you. Wait and retry.
- **The clipboard copy did nothing.** It is best-effort and needs a clipboard,
  which plain SSH sessions and headless containers do not have. The link is still
  printed on stdout; pass `--no-copy` to skip the attempt.
