# Cairn

**Cairn is an AI-native artifact-sharing service** — a pastebin / gist / requestbin
for the agent era. Humans post from the CLI and web; agents read, create, comment,
and react over MCP. Every artifact gets a short shareable URL with provenance,
reactions, comments, and a TTL. See [docs/DESIGN.md](docs/DESIGN.md) for the full
product design brief.

## Architecture Context

- Architecture Decision Records are in docs/adrs/
- Specifications are in docs/openspec/specs/
- The product design brief is in docs/DESIGN.md

This project follows **spec-driven development** (the `sdd` plugin). Read the ADRs
and specs before proposing structural changes. When implementing code governed by a
spec, leave a governing comment: `// Governing: ADR-XXXX (desc), SPEC-XXXX REQ "…"`.

### SDD Configuration

#### Tracker
- **Type**: github
- **Owner**: joestump
- **Repo**: cairn

#### Branch Conventions
- **Enabled**: true
- **Prefix**: feature
- **Epic Prefix**: epic
- **Slug Max Length**: 50

#### PR Conventions
- **Enabled**: true
- **Close Keyword**: Closes
- **Ref Keyword**: Part of
- **Include Spec Reference**: true
