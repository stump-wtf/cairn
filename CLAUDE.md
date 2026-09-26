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

### A broken test is not a false test — it falls through to `else`

`[ ... ]` does not return "false" when the *expression itself* is invalid. It
**errors**, and inside an `if` that error lands in the `else` branch:

```bash
if [ "$a" \> "$b" ]; then    # zsh: "condition expected: >" — the test ERRORS
  echo "a is newer"
else
  echo "b is newer"           # ...so this runs, asserting the inverse
fi
```

This happened in this repo while dating the release chain against a tag. The
comparison was invalid, the `else` fired, and the script printed a fluent,
plausible, **exactly inverted** conclusion — which was nearly reported as fact.

That makes it the worst member of this family. A masked exit code or an empty
grep at least *looks* suspicious. A confident wrong verdict does not.

**Validate a comparison against known inputs before trusting it on unknown
ones**, the same way the binary gate proves it can see before it reports
absence:

```bash
later() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort | tail -1)" = "$1" ] && [ "$1" != "$2" ]; }
later 2026-09-12 2026-07-15 && echo YES || echo NO   # expect YES
later 2026-07-15 2026-09-12 && echo YES || echo NO   # expect NO
later 2026-09-12 2026-09-12 && echo YES || echo NO   # expect NO
```

### `\s`, `\S` and `\d` are GNU extensions, and BSD fails them silently

macOS `grep -E` does not support them. The pattern does not error; it simply
**matches nothing**:

```bash
git grep -nE '^\s*var\s+version'                      # macOS: zero hits, even where it exists
git grep -nE '^[[:space:]]*var[[:space:]]+version'    # portable
```

This cost a wrong diagnosis here: two probes came back empty, the emptiness was
blamed on the git pathspec, and the real cause was `\s`. A positive control
found it — the same query shape matched a string known to be present, which
proved the method worked and the pattern did not.

Use POSIX classes, `-F` for fixed strings, or `grep -P` where it exists.

### Never print a conclusion you have not checked the data against

Guard text like `(empty above = free to claim)` or `(nothing listed = only X
changed)` prints unconditionally, so the moment the list is non-empty it
asserts the opposite of what is on screen. This shipped four times in one
session, once directly above a list of thirteen counter-examples.

Compute the claim from the data instead of narrating it:

```bash
n=$(some-command | wc -l | tr -d ' ')
if [ "$n" -eq 0 ]; then echo "none found"; else echo "$n found:"; some-command; fi
```
