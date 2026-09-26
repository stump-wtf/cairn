---
status: draft
date: 2026-09-13
implements: [ADR-0020]
requires: [SPEC-0010]
---

# SPEC-0015: Embedded Documentation Serving

## Overview

`cairnd` serves the published documentation itself. The Docusaurus bundle built
from `website/` (SPEC-0010, ADR-0014) is embedded in the server binary behind the
`docs` build tag and served at `/docs/*` from the same process that serves the
API, the MCP endpoint, and the web app shell. A `cairn-docs` container or any
other sidecar is no longer part of the runtime: one binary, a Postgres, an S3
bucket.

This capability realizes **ADR-0020**. It defines what routes the binary serves,
what configuration selects them, what a self-hoster observes, and how the
without-Node build degrades.

## Requirements

### Requirement: Embedded Bundle Build

The packaged `cairnd` binary MUST embed the Docusaurus build output at
`website/build/` via `go:embed`, compiled with the `docs` build tag. A build
without the tag MUST compile and pass `go test` with no Node toolchain present,
serving a stub page that states the binary was built without embedded docs.

#### Scenario: Tagged build embeds the bundle

- **WHEN** `go build -tags docs` runs after `npm run build` in `website/`
- **THEN** the resulting binary contains the bundle and serves `/docs/` with the
  generated record pages

#### Scenario: Untagged build in a Node-less environment

- **WHEN** `go build ./...` and `go test ./...` run on a machine without Node
- **THEN** both succeed, and the binary serves the stub page at `/docs/`

#### Scenario: Stub is honest

- **WHEN** the stub page is served
- **THEN** its body names the `docs` build tag, so an operator can tell a
  packaging mistake from a serving bug

### Requirement: Route and Content Serving

`cairnd` MUST serve the embedded bundle under the `/docs/` path prefix, with the
prefix matching the bundle's configured base path. The server MUST NOT claim
routes outside `/docs/*`; the API, MCP, and app shell routes are unchanged.

#### Scenario: A record page renders

- **WHEN** `GET /docs/` and a deep record page (for example `/docs/decisions/adr-0020-single-binary-runtime-embedded-docs/`) are fetched
- **THEN** the response bodies contain the expected page prose, asserted on
  content — never on status codes, because the SPA returns 200 for every path

#### Scenario: Static assets under the prefix

- **WHEN** a page's asset URLs are requested
- **THEN** each resolves under `/docs/` with its correct content type, so no
  first-paint breakage of the kind the cloud01 edge once shipped

#### Scenario: Unknown path under the prefix

- **WHEN** `GET /docs/no-such-page/` is fetched
- **THEN** the bundle's own 404 page is served with a 404 status, not the SPA's
  catch-all 200

### Requirement: Configuration

Docs serving MUST be governed by an explicit, documented configuration knob
(`CAIRN_DOCS_ENABLED`, default enabled in the packaged build), so an operator can
turn the surface off without rebuilding. The knob MUST be listed in the
self-hosting guide and the compose files in the same change.

#### Scenario: Disabled at runtime

- **WHEN** `CAIRN_DOCS_ENABLED=false` and `GET /docs/` is fetched
- **THEN** the response is a 404 and no bundle bytes are served

### Requirement: Deployment Convergence

The single-image runtime MUST be the only supported self-host path: the
published guide (`website/docs/guides/self-hosting.md`) MUST be the one
documented deployment path, describing one `cairnd` service with embedded
docs. The `ghcr.io/stump-wtf/cairn` pull contract MUST keep working across
the change — same name, same env surface, same ports.

#### Scenario: Compose bring-up

- **WHEN** a fresh self-hoster follows the guide's compose path
- **THEN** the stack is cairnd + Postgres + S3 + Caddy, and the guide pages are
  served by cairnd at the same origin

#### Scenario: Existing pullers unaffected

- **WHEN** `docker pull ghcr.io/stump-wtf/cairn` runs after the change
- **THEN** the image pulls anonymously and starts with the documented env

#### Scenario: cloud01 migration

- **WHEN** the converge lands on cloud01
- **THEN** the `cairn-docs` container and its dedicated edge route are gone, the
  edge proxies `/docs/*` to `cairnd`, and the live guide renders identical
  content from the binary

### Requirement: Verification Honesty

Any check that "the docs are served" MUST assert on response body content. The
Docusaurus bundle returns 200 for every path including nonexistent ones, so a
status-code assertion MUST be treated as unverified.

#### Scenario: Status-code-only check rejected

- **WHEN** a test or CI job claims docs serving works using only an HTTP status
- **THEN** the check does not count as verification; content assertions are
  required by this spec

## Consequences

- Docs fixes ship with the server, not independently; the GitHub Pages twin
  (ADR-0014) remains the independent public copy.
- The Docker build gains an ordered Node-then-Go stage; the untagged fallback
  keeps Go-only CI honest.
- The `cairn` CLI is unaffected (SPEC-0008): separate binary, separate release.

## More Information

- ADR-0020 (this spec's decision), ADR-0014 (site content pipeline).
- Epic: `stump.wtf/cairn#246` (unlinked — repository hosts are excluded from
  the rendered record by SPEC-0010 REQ "No Repository Links").
