---
title: Specifications
sidebar_position: 6
---

# Specifications

Each capability is an **OpenSpec** specification — a paired `spec.md` (normative
requirements, RFC 2119) and `design.md` (architecture + rationale, with diagrams).
Web-facing specs carry a Security section (auth-by-default endpoint tables); UI-facing
specs carry an Accessibility section. Together the nine specs hold **187 requirements
across 303 scenarios**.

| SPEC | Capability | Implements | Epic |
|------|------------|------------|------|
| [SPEC-0001](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/web-app-shell-and-bin/spec.md) | Web app shell & the Bin | ADR-0011, ADR-0003 | [#31](https://github.com/joestump/cairn/issues/31) |
| [SPEC-0002](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/artifact-core-and-share-types/spec.md) | Artifact core & share-type registry | ADR-0001/02/08/05 | [#7](https://github.com/joestump/cairn/issues/7) |
| [SPEC-0003](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/artifact-viewers/spec.md) | Artifact viewers (md/code/image/file/bundle) | ADR-0002, ADR-0011 | [#1](https://github.com/joestump/cairn/issues/1) |
| [SPEC-0004](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/trajectory-share/spec.md) | Trajectory share | ADR-0009 | [#6](https://github.com/joestump/cairn/issues/6) |
| [SPEC-0005](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/webhook-inspector/spec.md) | Webhook inspector | ADR-0010 | [#21](https://github.com/joestump/cairn/issues/21) |
| [SPEC-0006](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/annotations/spec.md) | Annotations — reactions & comments | ADR-0006 | [#11](https://github.com/joestump/cairn/issues/11) |
| [SPEC-0007](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/mcp-server-and-oauth/spec.md) | MCP server & OAuth | ADR-0004, ADR-0003 | [#22](https://github.com/joestump/cairn/issues/22) |
| [SPEC-0008](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/cli/spec.md) | The `cairn` CLI | ADR-0003 | [#33](https://github.com/joestump/cairn/issues/33) |
| [SPEC-0009](https://github.com/joestump/cairn/blob/main/docs/openspec/specs/provenance-access-retention/spec.md) | Provenance, access & retention | ADR-0007, ADR-0005 | [#27](https://github.com/joestump/cairn/issues/27) |

Each spec's `spec.md` and `design.md` live together under
[`docs/openspec/specs/`](https://github.com/joestump/cairn/tree/main/docs/openspec/specs).
The build is tracked as [GitHub issues](https://github.com/joestump/cairn/issues)
(9 epics + 36 story-sized issues) grouped by
[milestone](https://github.com/joestump/cairn/milestones).
