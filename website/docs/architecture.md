---
title: Architecture
sidebar_position: 5
---

# Architecture (ADRs)

Cairn's architecture is captured as **Architecture Decision Records** in
[MADR](https://adr.github.io/madr/) format. Each records one decision — the context,
the options weighed, the choice, and its consequences — and they form a
forward-only graph rooted at ADR-0001.

## The house stack

| Layer | Choice |
|-------|--------|
| Backend & CLI | **Go** — single static binary, self-hostable |
| API | **REST/JSON** for CRUD, **SSE** for live webhook & trajectory streams |
| Metadata | **PostgreSQL** — artifacts, annotations, provenance, access, workspaces |
| Bodies | **S3-compatible object storage**, content-addressed by **SHA-256** |
| Identifiers | short opaque **base62** public ids |
| Web | server-rendered **Go html/template + HTMX + Alpine.js** |
| Agent surface | a **Go MCP server**, authorized via **OAuth 2.1** (PKCE) |

## Decision records

| ADR | Decision |
|-----|----------|
| [ADR-0001](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0001-cairn-as-ai-native-artifact-store.md) | Cairn as an AI-native artifact store — product vision & domain model |
| [ADR-0002](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0002-extensible-share-type-model-viewer-registry.md) | Extensible share-type model with a viewer registry |
| [ADR-0003](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0003-triple-surface-parity-web-cli-mcp.md) | Triple-surface parity — web, CLI, and MCP over one core service |
| [ADR-0004](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0004-mcp-first-class-surface-oauth.md) | MCP as a first-class surface with OAuth authorization |
| [ADR-0005](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0005-short-opaque-identifiers-url-scheme.md) | Short opaque identifiers and the URL scheme |
| [ADR-0006](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0006-unified-annotation-layer.md) | Unified annotation layer — reactions & comments with typed anchors |
| [ADR-0007](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0007-provenance-link-access-default-expiry.md) | Provenance, link-based access control, and default expiry |
| [ADR-0008](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0008-storage-and-content-model.md) | Storage & content model — blob bodies, metadata, content addressing |
| [ADR-0009](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0009-trajectory-capture-and-otel-inspired-span-model.md) | Trajectory capture and the OTel-inspired span model |
| [ADR-0010](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0010-live-webhook-endpoints-and-real-time-stream-capture.md) | Live webhook endpoints and real-time stream capture |
| [ADR-0011](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0011-frontend-architecture-and-the-unified-app-shell.md) | Frontend architecture and the unified app shell |
| [ADR-0012](https://github.com/joestump/cairn/blob/main/docs/adrs/ADR-0012-backend-platform-and-api-shape.md) | Backend platform and API shape |

The ADRs are the source of truth; this page indexes them. Read them in the repo at
[`docs/adrs/`](https://github.com/joestump/cairn/tree/main/docs/adrs).
