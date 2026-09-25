#!/usr/bin/env bash
#
# Render The Homebrew Formula For A Release
#
# The tap-bump job writes stump.wtf/homebrew-tap's Formula/cairn.rb by
# rendering packaging/homebrew/cairn.rb.tmpl, rather than editing a formula
# already in the tap. Editing in place needed a formula to exist before the
# first release, and a placeholder can't pass the tap's own CI: `brew audit
# --online` rejects its 404ing urls, and `--strict` rejects the `version` line
# the in-place edit keyed on. Rendering needs neither, so the first release
# creates the formula and every later one replaces it.
#
# Every input is validated, and the output is checked for leftover
# placeholders. A formula with an empty or mistyped checksum fails `brew
# install` for every user rather than for us, so this refuses to write one.
#
# Usage:
#   scripts/render-brew-formula.sh <version> <sha256-darwin-arm64> \
#     <sha256-darwin-amd64> <sha256-linux-arm64> <sha256-linux-amd64> > cairn.rb
#
# @joestump 09/25/2026 - Created for cairn#183, replacing the tap-bump job's
# in-place sed over a formula that had to pre-exist in the tap.

set -euo pipefail

fail() { echo "::error::$*" >&2; exit 1; }

[ "$#" -eq 5 ] || fail "usage: $0 <version> <sha256-darwin-arm64> <sha256-darwin-amd64> <sha256-linux-arm64> <sha256-linux-amd64>"

here="$(cd "$(dirname "$0")" && pwd)"
tmpl="${CAIRN_FORMULA_TEMPLATE:-$here/../packaging/homebrew/cairn.rb.tmpl}"
[ -f "$tmpl" ] || fail "template not found: $tmpl"

ver="$1"
# goreleaser's {{ .Version }}: no leading v, optional pre-release suffix.
printf '%s' "$ver" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' \
  || fail "not a release version (want 1.2.3, no leading v): '$ver'"

names=(DARWIN_ARM64 DARWIN_AMD64 LINUX_ARM64 LINUX_AMD64)
shas=("$2" "$3" "$4" "$5")
for i in 0 1 2 3; do
  printf '%s' "${shas[$i]}" | grep -Eq '^[0-9a-f]{64}$' \
    || fail "sha256 for ${names[$i]} is not 64 lowercase hex characters: '${shas[$i]}'"
done

out="$(sed \
  -e "s/@VERSION@/${ver}/g" \
  -e "s/@SHA256_DARWIN_ARM64@/${shas[0]}/g" \
  -e "s/@SHA256_DARWIN_AMD64@/${shas[1]}/g" \
  -e "s/@SHA256_LINUX_ARM64@/${shas[2]}/g" \
  -e "s/@SHA256_LINUX_AMD64@/${shas[3]}/g" \
  "$tmpl")"

# sed exits 0 whether or not a pattern matched, so check the result, not sed.
left="$(printf '%s\n' "$out" | grep -c '@[A-Z0-9_]*@' || true)"
[ "$left" -eq 0 ] || fail "$left line(s) still carry a placeholder after rendering; the template and this script disagree"
for i in 0 1 2 3; do
  printf '%s\n' "$out" | grep -q "sha256 \"${shas[$i]}\"" || fail "rendered formula is missing the ${names[$i]} checksum"
done
printf '%s\n' "$out" | grep -q "/download/v${ver}/cairn_${ver}_" || fail "rendered formula does not name version ${ver} in its urls"

printf '%s\n' "$out"
