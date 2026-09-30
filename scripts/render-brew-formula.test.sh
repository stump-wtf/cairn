#!/usr/bin/env bash
#
# Tests For render-brew-formula.sh
#
# The renderer runs once per release, on the path to every user's `brew
# install`, so the cases that matter most are the refusals: each must fail for
# its own reason, which is why they match the error text and not just the exit
# code.
#
# @joestump 09/25/2026 - Created for cairn#183.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
render="$here/render-brew-formula.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

A="$(printf 'a%.0s' $(seq 64))"
B="$(printf 'b%.0s' $(seq 64))"
C="$(printf 'c%.0s' $(seq 64))"
D="$(printf 'd%.0s' $(seq 64))"

pass=0
failures=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1"; failures=$((failures + 1)); }

expect_fail() {
  local name="$1" want="$2"; shift 2
  if out="$("$@" 2>&1)"; then
    bad "$name: expected failure, got success"
  elif printf '%s' "$out" | grep -q -F "$want"; then
    ok "$name"
  else
    bad "$name: failed, but not with \"$want\": $out"
  fi
}

# The good case, checked by content: every url names the version, each
# checksum lands on its own architecture, and no placeholder survives.
if "$render" 0.1.1 "$A" "$B" "$C" "$D" > "$work/cairn.rb" 2> "$work/err"; then
  f="$work/cairn.rb"
  if [ "$(grep -c '/download/v0.1.1/cairn_0.1.1_' "$f")" -eq 4 ] \
    && grep -A1 'cairn_0.1.1_darwin_arm64' "$f" | grep -q "sha256 \"$A\"" \
    && grep -A1 'cairn_0.1.1_darwin_amd64' "$f" | grep -q "sha256 \"$B\"" \
    && grep -A1 'cairn_0.1.1_linux_arm64' "$f" | grep -q "sha256 \"$C\"" \
    && grep -A1 'cairn_0.1.1_linux_amd64' "$f" | grep -q "sha256 \"$D\"" \
    && ! grep -q '@[A-Z0-9_]*@' "$f" \
    && ! grep -Eq '^[[:space:]]*version ' "$f"; then
    ok "a release renders with each checksum on its own architecture"
  else
    bad "rendered formula has the wrong content:"; sed 's/^/     /' "$f"
  fi
  if command -v ruby >/dev/null 2>&1; then
    if ruby -c "$f" >/dev/null 2>&1; then ok "the rendered formula is valid Ruby"; else bad "ruby -c rejects the rendered formula"; fi
  else
    echo "note ruby not installed; Ruby syntax check not run"
  fi
else
  bad "good render failed: $(cat "$work/err")"
fi

expect_fail "a leading v is refused" "not a release version" "$render" v0.1.1 "$A" "$B" "$C" "$D"
expect_fail "a non-version is refused" "not a release version" "$render" latest "$A" "$B" "$C" "$D"
expect_fail "an empty checksum is refused" "sha256 for LINUX_AMD64" "$render" 0.1.1 "$A" "$B" "$C" ""
expect_fail "a short checksum is refused" "sha256 for DARWIN_AMD64" "$render" 0.1.1 "$A" "abc123" "$C" "$D"
expect_fail "an uppercase checksum is refused" "sha256 for DARWIN_ARM64" "$render" 0.1.1 "$(printf 'A%.0s' $(seq 64))" "$B" "$C" "$D"
# Computed here rather than copied from the renderer, so a typo in its
# constant fails this case instead of agreeing with it.
if command -v sha256sum >/dev/null 2>&1; then E="$(printf '' | sha256sum | cut -d' ' -f1)"
else E="$(printf '' | shasum -a 256 | cut -d' ' -f1)"; fi
expect_fail "an empty download's checksum is refused" "sha256 for LINUX_ARM64 is the checksum of an empty file" "$render" 0.1.1 "$A" "$B" "$E" "$D"
expect_fail "a missing argument is refused" "usage:" "$render" 0.1.1 "$A" "$B" "$C"

# A template placeholder this script does not know must stop the render,
# not ship as a literal @...@ in someone's formula.
cp "$here/../packaging/homebrew/cairn.rb.tmpl" "$work/t.tmpl"
echo '  # @UNKNOWN_FIELD@' >> "$work/t.tmpl"
expect_fail "an unknown template placeholder is refused" "still carry a placeholder" \
  env CAIRN_FORMULA_TEMPLATE="$work/t.tmpl" "$render" 0.1.1 "$A" "$B" "$C" "$D"

echo "$pass passed, $failures failed"
[ "$failures" -eq 0 ]
