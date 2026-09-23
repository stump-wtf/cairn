#!/usr/bin/env bash
#
# Each Release Archive Holds Its Own Binary, And Only That
#
# A release carries two kinds of archive (cairn#361): `cairn_*`, the CLI, and
# `cairnd_*`, the server. This script fails if either ever holds the other's
# binary, anything but its binary plus LICENSE, or is missing for a platform
# the release promises. It runs over goreleaser's dist/ from a snapshot build
# BEFORE the real release job publishes, because goreleaser has no post-archive
# hook in the open-source edition: by the time a real `release` has archived,
# it has also uploaded.
#
# It complements scripts/verify-cli-artifact.sh rather than replacing it. That
# script is a per-binary goreleaser hook and answers "did the CLI binary pick
# up server code?". This one answers "did the packaging put the right binary
# in the right archive?", which is a config property: an archive with no `ids`
# takes every build, so adding the cairnd build without scoping the CLI archive
# would have shipped both binaries under the CLI's name.
#
# The cairnd check carries a POSITIVE CONTROL for the same reason the CLI gate
# does: a marker search that cannot find the server's own packages in the
# server binary is not measuring anything, and its "no CLI code" result would
# be vacuous.
#
# Usage:
#   scripts/verify-release-archives.sh [dist-dir]     (default: dist)
#
# @joestump 09/23/2026 - Created for cairn#361, alongside the cairnd build.

set -euo pipefail

DIST="${1:-dist}"
MODULE="github.com/stump-wtf/cairn"

CLI_PLATFORMS=(linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64)
SERVER_PLATFORMS=(linux_amd64 linux_arm64 darwin_amd64 darwin_arm64)

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

# find_archive <prefix> <platform>: the one archive for that platform, or empty.
find_archive() {
  local prefix="$1" platform="$2" f
  for f in "$DIST"/"${prefix}"_*_"${platform}".tar.gz "$DIST"/"${prefix}"_*_"${platform}".zip; do
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

echo "==> CLI archives (cairn_*)"
for p in "${CLI_PLATFORMS[@]}"; do
  a="$(find_archive cairn "$p")"
  [ -n "$a" ] || fail "no cairn archive for $p in $DIST"
  bin=cairn
  case "$p" in windows_*) bin=cairn.exe ;; esac
  check_archive "$a" "$bin"
done

echo "==> server archives (cairnd_*)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for p in "${SERVER_PLATFORMS[@]}"; do
  a="$(find_archive cairnd "$p")"
  [ -n "$a" ] || fail "no cairnd archive for $p in $DIST"
  case "$a" in *.tar.gz) ;; *) fail "$(basename "$a"): cairnd ships as tar.gz only" ;; esac
  check_archive "$a" cairnd

  # The bytes, not just the name: a server binary that is really the CLI (or
  # carries it) would pass the member check above.
  rm -rf "$tmp/x" && mkdir -p "$tmp/x"
  tar -xzf "$a" -C "$tmp/x" cairnd
  control="$(grep -a -c -F "${MODULE}/internal/httpapi" "$tmp/x/cairnd" || true)"
  [ "$control" -gt 0 ] || fail "positive control failed: '${MODULE}/internal/httpapi' does not appear in $(basename "$a")'s cairnd, so this check cannot tell a server from anything else"
  cli="$(grep -a -c -F "${MODULE}/internal/clicmd" "$tmp/x/cairnd" || true)"
  [ "$cli" -eq 0 ] || fail "CLI CODE IN THE SERVER: '${MODULE}/internal/clicmd' appears in $(basename "$a")'s cairnd"
  echo "     server bytes ok (httpapi present, clicmd absent)"
done

# No archive the two lists above did not account for, e.g. a third build that
# picked up a default archive, or a cairnd for windows nobody asked for.
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
