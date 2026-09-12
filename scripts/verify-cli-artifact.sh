#!/usr/bin/env bash
#
# The Shipped CLI Must Not Contain The Server
#
# cairn's source is private and the hosted service may be sold, so the only
# artifact that goes out publicly is the `cairn` CLI. This script fails the
# build if the server ever ends up inside it. It runs in CI on every tag, not
# as a one-off inspection, because the way this breaks is a future import in
# a CLI package quietly pulling a server package behind it.
#
# Two independent gates, because each catches what the other cannot:
#
#   1. The import graph (authoritative). `go list -deps ./cmd/cairn` is
#      evaluated at build time and cannot be defeated by stripping. This gate
#      is an ALLOWLIST, not a denylist: a denylist silently passes a server
#      package invented after the list was written, which is exactly the
#      failure mode a gate like this exists to prevent.
#
#   2. The built binary (corroborating). `strings` over the artifact that
#      actually ships. Paired with a POSITIVE CONTROL, for a reason worth
#      stating: `go tool nm` looks like the obvious tool here and is useless,
#      because the release ldflags carry `-s -w` and strip the symbol table.
#      An nm-based gate reports "no server symbols" for a binary containing
#      the entire server. A check that cannot tell "absent" from
#      "unmeasurable" is worse than no check, so this one proves it can see
#      the CLI's own packages before it believes the server's are missing.
#
# Usage:
#   scripts/verify-cli-artifact.sh [path-to-built-cairn-binary]
#
# The binary argument is optional; without it only gate 1 runs (useful as a
# fast pre-commit check). CI passes a real goreleaser artifact.
#
# @joestump 09/12/2026 - Created. Gate 1 written as an allowlist and gate 2
# given a positive control after an nm-based check returned a clean bill of
# health on a stripped binary — including for packages known to be present.

set -euo pipefail

MODULE="github.com/stump-wtf/cairn"

# The complete set of this module's packages the CLI is allowed to reach.
# Adding a line here is a deliberate act: it must be client-only code.
ALLOWED_PACKAGES=(
  "${MODULE}/cmd/cairn"
  "${MODULE}/internal/cliclient"
  "${MODULE}/internal/clicmd"
  "${MODULE}/internal/cliconfig"
  "${MODULE}/internal/cliexit"
)

# Server package fragments asserted absent from the shipped bytes. This list
# is corroborating only — gate 1 is what actually holds the line — so it does
# not need to be exhaustive to be useful.
SERVER_MARKERS=(
  "${MODULE}/internal/httpapi"
  "${MODULE}/internal/store"
  "${MODULE}/internal/db"
  "${MODULE}/internal/oauth"
  "${MODULE}/internal/objectstore"
  "${MODULE}/internal/session"
  "${MODULE}/internal/webhook"
  "${MODULE}/internal/artifact"
  "${MODULE}/internal/annotation"
  "${MODULE}/internal/mcpsession"
)

fail() { echo "::error::$*" >&2; exit 1; }

# ---- Gate 1: the import graph -------------------------------------------
echo "==> gate 1: import graph of ./cmd/cairn"

deps="$(go list -deps ./cmd/cairn)"
own="$(printf '%s\n' "$deps" | grep "^${MODULE}" || true)"

[ -n "$own" ] || fail "go list returned no ${MODULE} packages — the graph could not be read, so this gate proved nothing."

violations=0
while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  allowed=0
  for ok in "${ALLOWED_PACKAGES[@]}"; do
    [ "$pkg" = "$ok" ] && { allowed=1; break; }
  done
  if [ "$allowed" -eq 0 ]; then
    echo "  DISALLOWED: $pkg"
    violations=$((violations + 1))
  else
    echo "  ok: $pkg"
  fi
done <<< "$own"

[ "$violations" -eq 0 ] || fail "the cairn CLI imports $violations package(s) outside the client allowlist. The CLI must not reach server code; if a type is genuinely shared, move it to a client-safe package rather than widening this list."

# ---- Gate 2: the built binary -------------------------------------------
BIN="${1:-}"
if [ -z "$BIN" ]; then
  echo "==> gate 2: skipped (no binary given)"
  echo "PASS: import graph is client-only"
  exit 0
fi

[ -f "$BIN" ] || fail "binary not found: $BIN"
echo "==> gate 2: shipped binary $BIN"

# Positive control FIRST. If we cannot see the CLI's own packages, `strings`
# is not telling us anything about this binary and every later assertion
# would be a false negative.
control_hits="$(strings -a "$BIN" | grep -c "${MODULE}/internal/clicmd" || true)"
[ "$control_hits" -gt 0 ] || fail "positive control failed: '${MODULE}/internal/clicmd' does not appear in $BIN, so this check cannot distinguish a clean binary from an unreadable one. Do not interpret the result below as a pass."
echo "  positive control ok: clicmd appears $control_hits time(s)"

for marker in "${SERVER_MARKERS[@]}"; do
  hits="$(strings -a "$BIN" | grep -c "$marker" || true)"
  if [ "$hits" -gt 0 ]; then
    fail "SERVER CODE IN THE SHIPPED CLI: '$marker' appears $hits time(s) in $BIN"
  fi
done
echo "  no server package paths present"

# Embedded server assets: migrations are the loudest tell, since internal/db
# embeds migrations/*.sql and internal/httpapi embeds templates and assets.
sql_hits="$(strings -a "$BIN" | grep -c "CREATE TABLE" || true)"
[ "$sql_hits" -eq 0 ] || fail "migration SQL found in $BIN ($sql_hits 'CREATE TABLE' strings) — a server package is embedded."
echo "  no embedded migration SQL"

echo "PASS: $BIN is client-only"
