# Security policy

## Reporting a vulnerability

Report it privately. Don't put details of a vulnerability in a public issue,
comment or pull request.

1. Open the repository's **Security** tab on GitHub
   (https://github.com/stump-wtf/cairn/security). If it offers **Report a
   vulnerability**, use it. That form is private between you and the maintainers.
2. If that button isn't there, open an issue at
   https://github.com/stump-wtf/cairn/issues titled `Security contact request`,
   with **no details** about the problem. A maintainer will reply with a private
   channel to send them through.

Please include:

- the version: the image tag plus digest, or the commit you built;
- the surface: web, CLI, MCP or the REST API;
- the steps to reproduce, and what an attacker gains.

Never send real credentials, even in a private report. That covers API tokens,
OAuth or session cookies, webhook ingest URLs, and the contents of anyone
else's artifacts. Use `<redacted>` or a throwaway instance.

## Supported versions

Only the latest release gets security fixes. Today that is the `v0.1.x` line,
published as the container image `ghcr.io/stump-wtf/cairn`. If you run an older
image, upgrade before reporting, and check whether the problem is still there.

## Scope

In scope: the Cairn server (`cairnd`, including its web UI, REST API and MCP
endpoint), the `cairn` CLI, and the container image.

Out of scope: problems in an instance's own setup, such as a reverse proxy,
the identity provider, Postgres or the object store, unless Cairn's defaults
or documentation caused them.
