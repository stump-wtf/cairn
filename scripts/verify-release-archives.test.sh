#!/usr/bin/env bash
#
# Tests For verify-release-archives.sh
#
# Builds fake dist/ directories and asserts the verifier passes the good one and
# fails each bad one. The failing cases are the point: a release gate that is
# only ever seen passing has not been shown to check anything. Each bad case
# must fail for its OWN reason, so the test matches the error text rather than
# just a non-zero exit.
#
# The "binaries" are small files carrying the package paths the verifier greps
# for, which is all it inspects; no Go build is needed, so this runs in
# `make check` in a second or two.
#
# @joestump 09/23/2026 - Created for cairn#361.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
verify="$here/verify-release-archives.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

MODULE="github.com/stump-wtf/cairn"
V="9.9.9"

# make_zip <out.zip> <dir> <files...>: python3 rather than `zip`, which slim
# CI images tend not to have.
make_zip() {
  local out="$1" dir="$2"; shift 2
  python3 - "$out" "$dir" "$@" <<'PY'
import os, sys, zipfile
out, d, names = sys.argv[1], sys.argv[2], sys.argv[3:]
with zipfile.ZipFile(out, "w") as z:
    for n in names:
        z.write(os.path.join(d, n), n)
PY
}

# good_dist <dir>: a dist/ shaped like a correct release.
good_dist() {
  local d="$1" p
  mkdir -p "$d/src"
  echo "MIT" > "$d/src/LICENSE"
  printf 'x %s/internal/clicmd y\n' "$MODULE" > "$d/src/cairn"
  cp "$d/src/cairn" "$d/src/cairn.exe"
  printf 'x %s/internal/httpapi y\n' "$MODULE" > "$d/src/cairnd"
  for p in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
    tar -czf "$d/cairn_${V}_${p}.tar.gz" -C "$d/src" cairn LICENSE
    tar -czf "$d/cairnd_${V}_${p}.tar.gz" -C "$d/src" cairnd LICENSE
  done
  for p in windows_amd64 windows_arm64; do
    make_zip "$d/cairn_${V}_${p}.zip" "$d/src" cairn.exe LICENSE
  done
  write_sums "$d"
}

write_sums() {
  local d="$1" f
  : > "$d/checksums.txt"
  for f in "$d"/*.tar.gz "$d"/*.zip; do
    [ -f "$f" ] || continue
    echo "0000  $(basename "$f")" >> "$d/checksums.txt"
  done
}

pass=0
failures=0

expect_pass() {
  local name="$1" d="$2"
  if out="$("$verify" "$d" 2>&1)"; then
    echo "ok   $name"; pass=$((pass + 1))
  else
    echo "FAIL $name: expected pass, got:"; echo "$out" | sed 's/^/     /'
    failures=$((failures + 1))
  fi
}

expect_fail() {
  local name="$1" d="$2" want="$3"
  if out="$("$verify" "$d" 2>&1)"; then
    echo "FAIL $name: expected failure, verifier passed"
    failures=$((failures + 1))
  elif printf '%s' "$out" | grep -q -F "$want"; then
    echo "ok   $name"; pass=$((pass + 1))
  else
    echo "FAIL $name: failed, but not with \"$want\":"; echo "$out" | sed 's/^/     /'
    failures=$((failures + 1))
  fi
}

# fresh <name>: a new good dist/ to break one way.
fresh() { local d="$work/$1"; good_dist "$d"; echo "$d"; }

d="$(fresh good)"
expect_pass "a correct release passes" "$d"

d="$(fresh cli-carries-server)"
tar -czf "$d/cairn_${V}_linux_amd64.tar.gz" -C "$d/src" cairn cairnd LICENSE
expect_fail "the CLI archive holding cairnd fails" "$d" "must hold exactly 'cairn' and 'LICENSE'"

d="$(fresh server-carries-cli)"
tar -czf "$d/cairnd_${V}_darwin_arm64.tar.gz" -C "$d/src" cairnd cairn LICENSE
expect_fail "the server archive holding cairn fails" "$d" "must hold exactly 'cairnd' and 'LICENSE'"

d="$(fresh zip-carries-server)"
make_zip "$d/cairn_${V}_windows_amd64.zip" "$d/src" cairn.exe cairnd LICENSE
expect_fail "a windows CLI zip holding cairnd fails" "$d" "must hold exactly 'cairn.exe' and 'LICENSE'"

d="$(fresh no-license)"
tar -czf "$d/cairnd_${V}_linux_arm64.tar.gz" -C "$d/src" cairnd
expect_fail "an archive without LICENSE fails" "$d" "must hold exactly 'cairnd' and 'LICENSE'"

d="$(fresh missing-platform)"
rm "$d/cairnd_${V}_darwin_amd64.tar.gz"; write_sums "$d"
expect_fail "a missing cairnd platform fails" "$d" "no cairnd archive for darwin_amd64"

d="$(fresh server-is-really-cli)"
cp "$d/src/cairn" "$d/src/cairnd"
tar -czf "$d/cairnd_${V}_linux_amd64.tar.gz" -C "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "a cairnd without server code fails the positive control" "$d" "positive control failed"

d="$(fresh server-with-cli-code)"
printf 'x %s/internal/httpapi %s/internal/clicmd\n' "$MODULE" "$MODULE" > "$d/src/cairnd"
tar -czf "$d/cairnd_${V}_linux_amd64.tar.gz" -C "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "a cairnd carrying CLI code fails" "$d" "CLI CODE IN THE SERVER"

d="$(fresh unexpected-archive)"
tar -czf "$d/cairnd_${V}_windows_amd64.tar.gz" -C "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "an archive the release does not account for fails" "$d" "an archive exists that this release does not account for"

d="$(fresh unlisted-checksum)"
grep -v "cairn_${V}_linux_arm64" "$d/checksums.txt" > "$d/c" && mv "$d/c" "$d/checksums.txt"
expect_fail "an archive missing from checksums.txt fails" "$d" "checksums.txt does not list"

mkdir -p "$work/empty"
expect_fail "an empty dist fails" "$work/empty" "no cairn archive for linux_amd64"
expect_fail "a missing dist fails" "$work/does-not-exist" "dist directory not found"

echo "$pass passed, $failures failed"
[ "$failures" -eq 0 ]
