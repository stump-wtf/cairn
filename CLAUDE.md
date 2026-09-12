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

## Shell rules for scripts and CI

**Never pipe a command whose exit status you depend on.** A pipeline's status is
the *last* stage's, so `set -e` walks straight past a failure:

```bash
set -euo pipefail
git push origin main | tail -3      # status is tail's: 0, even on a 403
echo "pushed"                       # ...so this runs anyway
```

This is not hypothetical. In a single session it reported a **403-denied `git
push` as a successfully created repository**, made a failed `git rebase` look
like it had succeeded, and elsewhere reported a deploy as `exit 0` while it
converged nothing. Every time, the output *looked* right — which is precisely
what makes it expensive: the failure is silent and downstream steps keep going.

Use one of these instead:

```bash
some-command                                  # simplest: don't pipe it
some-command > /tmp/out.log 2>&1 || { tail -20 /tmp/out.log; exit 1; }
set -o pipefail                               # pipeline fails if ANY stage does
```

### Before you reach for `PIPESTATUS`, read this

**`${PIPESTATUS[0]}` is bash. zsh names the array `pipestatus` and 1-indexes
it**, so on zsh the bash spelling expands to an **empty string** — not an error,
not a status:

```bash
some-command | tail -5
rc=${PIPESTATUS[0]}      # bash: the real status.  zsh: "" — silently nothing
[ "$rc" -ne 0 ] && ...   # zsh: errors on an empty operand, or never fires
```

That is the same defect one layer down: **the fix for a masked exit code
silently produces no exit code at all.** It happened in this repo — a script
printed `lint exit=` with nothing after the `=`, and the surrounding "Passed"
output was read as success. Reaching for `PIPESTATUS` from bash muscle memory
on a zsh login shell reintroduces the original bug while looking like the cure.

If you need a specific stage's status portably, don't: restructure so the
command you care about isn't in a pipeline.

One more: **`set -o pipefail` is bash/zsh only.** `sh` on Debian and Ubuntu is
dash, where it is an error — in an Ansible `shell:` task set
`executable: /bin/bash` before relying on it.

The same reasoning applies to any check: **a test that cannot distinguish
"the thing is absent" from "I could not measure it" reports success either way.**
See `scripts/verify-cli-artifact.sh`, whose binary gate carries a positive
control for exactly this reason.
