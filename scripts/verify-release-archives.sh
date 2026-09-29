#!/usr/bin/env bash
#
# The Release Archive Holds Its One Binary, And Only That
#
# A release carries one kind of archive: `cairn_*`, the whole program (ADR-0031
# folded the server into it; the former separate `cairnd_*` archives are gone).
# This script fails if an archive holds anything but its binary plus LICENSE,
# or is missing for a platform the release promises. It runs over goreleaser's
# dist/ from a snapshot build BEFORE the real release job publishes, because
# goreleaser has no post-archive hook in the open-source edition: by the time a
# real `release` has archived, it has also uploaded.
#
# "Which binary is in the archive" is a config property, not a build outcome:
# an archive with no `ids` takes every build, so adding a build without scoping
# the archive would ship binaries under the wrong name. That is what the
# member and unaccounted-archive checks below catch.
#
# The reproducibility check matters for the same reason it ever did: Harness
# pins this archive by URL and SHA-256, so its bytes must not depend on who
# built it. The build is -trimpath with mod_timestamp and .goreleaser.yaml
# pins every tar header; this is what notices if that stops holding.
#
# Usage:
#   scripts/verify-release-archives.sh [dist-dir]     (default: dist)
#
# @joestump 09/23/2026 - Created for cairn#361, alongside the cairnd build.
# @joestump 09/26/2026 - Check the tar headers too: owner, group and mode came
#   from the builder, so the pinned digest varied by machine.
# @joestump-agent 09/29/2026 - One binary (ADR-0031): the cairnd archive
#   checks and the positive control are gone with the second build; the
#   header checks now guard the cairn archive itself.

set -euo pipefail

DIST="${1:-dist}"

PLATFORMS=(linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64)

fail() { echo "::error::$*" >&2; exit 1; }

[ -d "$DIST" ] || fail "dist directory not found: $DIST"

# Member names, one per line, with directory entries and a leading ./ dropped.
list_members() {
  case "$1" in
    *.tar.gz) tar -tzf "$1" ;;
    *.zip)
      if command -v unzip >/dev/null 2>&1; then
        unzip -Z1 "$1"
      elif command -v python3 >/dev/null 2>&1; then
        python3 -c 'import sys, zipfile; print("\n".join(zipfile.ZipFile(sys.argv[1]).namelist()))' "$1"
      else
        fail "cannot list $1: neither unzip nor python3 is available, so this check would prove nothing"
      fi
      ;;
    *) fail "unknown archive type: $1" ;;
  esac | sed -e 's#^\./##' -e '/\/$/d' -e '/^$/d' | LC_ALL=C sort
}

# find_archive <platform>: the one archive for that platform, or empty.
find_archive() {
  local platform="$1" f
  for f in "$DIST"/cairn_*_"${platform}".tar.gz "$DIST"/cairn_*_"${platform}".zip; do
    [ -f "$f" ] && { echo "$f"; return 0; }
  done
  return 0
}

checked=0

check_archive() {
  local archive="$1" binary="$2" got want
  got="$(list_members "$archive")"
  want="$(printf '%s\n%s\n' "$binary" LICENSE | LC_ALL=C sort)"
  if [ "$got" != "$want" ]; then
    echo "  expected: $(echo "$want" | tr '\n' ' ')" >&2
    echo "  found:    $(echo "$got" | tr '\n' ' ')" >&2
    fail "$(basename "$archive") must hold exactly '$binary' and 'LICENSE'"
  fi
  echo "  ok: $(basename "$archive") = $binary + LICENSE"
  checked=$((checked + 1))
}

echo "==> cairn archives (cairn_*)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for p in "${PLATFORMS[@]}"; do
  a="$(find_archive "$p")"
  [ -n "$a" ] || fail "no cairn archive for $p in $DIST"
  bin=cairn
  case "$p" in windows_*) bin=cairn.exe ;; esac
  check_archive "$a" "$bin"

  # Reproducible headers: Harness pins this archive by SHA-256, so its bytes
  # must not depend on who built it. A tar header records each member's owner,
  # group and mode, and goreleaser copies any it is not given from the file on
  # disk, which made the same commit hash differently on a laptop (uid 501) and
  # in CI. .goreleaser.yaml pins all three; this is what notices if it stops.
  case "$a" in
    *.tar.gz)
      command -v python3 >/dev/null 2>&1 || fail "cannot read tar headers: python3 is not available, so the reproducibility check would prove nothing"
      bad="$(python3 - "$a" <<'PY'
import sys, tarfile
want = {"cairn": 0o755, "LICENSE": 0o644}
for m in tarfile.open(sys.argv[1]).getmembers():
    got = (m.uid, m.gid, m.uname, m.gname, m.mode)
    if got != (0, 0, "root", "root", want.get(m.name.lstrip("./"), -1)):
        print("%s=%d:%d/%s:%s/%o" % ((m.name,) + got), end=" ")
PY
)"
      [ -z "$bad" ] || fail "NOT REPRODUCIBLE: $(basename "$a") records builder-dependent headers (${bad% }); want root:root, cairn 0755, LICENSE 0644"
      echo "     headers ok (root:root, fixed modes)"
      ;;
  esac
done

# No archive the list above did not account for, e.g. a second build that
# picked up a default archive, or a stale artifact from an earlier config.
total=0
for f in "$DIST"/*.tar.gz "$DIST"/*.zip; do
  [ -f "$f" ] || continue
  total=$((total + 1))
done
[ "$total" -eq "$checked" ] || fail "$DIST holds $total archives but only $checked were expected; an archive exists that this release does not account for"

echo "==> checksums.txt"
[ -f "$DIST/checksums.txt" ] || fail "no checksums.txt in $DIST"
for f in "$DIST"/*.tar.gz "$DIST"/*.zip; do
  [ -f "$f" ] || continue
  grep -q "  $(basename "$f")\$" "$DIST/checksums.txt" || fail "checksums.txt does not list $(basename "$f")"
done
echo "  ok: every archive is listed"

echo "PASS: $checked archives, each holding only its own binary"
