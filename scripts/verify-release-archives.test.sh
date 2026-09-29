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
# @joestump 09/26/2026 - cairnd fixtures are built root-owned, and two cases
#   cover builder-dependent tar headers.

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

# make_tar <out.tar.gz> <dir> <uid> <user> <files...>: a tarball whose headers
# name that owner, with the modes goreleaser is configured to write (0755 for a
# binary, 0644 for LICENSE). python3 rather than `tar`, whose headers carry
# whoever runs the test, which is root on the CI runner and 501 on a laptop.
make_tar() {
  local out="$1" dir="$2" uid="$3" user="$4"; shift 4
  python3 - "$out" "$dir" "$uid" "$user" "$@" <<'PY'
import os, sys, tarfile
out, d, uid, user, names = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], sys.argv[5:]
with tarfile.open(out, "w:gz") as t:
    for n in names:
        ti = t.gettarinfo(os.path.join(d, n), n)
        ti.uid = ti.gid = uid
        ti.uname = ti.gname = user
        ti.mode = 0o644 if n == "LICENSE" else 0o755
        with open(os.path.join(d, n), "rb") as f:
            t.addfile(ti, f)
PY
}

# server_tar <out.tar.gz> <dir> <files...>: a cairnd archive as the release
# config writes it, root-owned.
server_tar() { local out="$1" dir="$2"; shift 2; make_tar "$out" "$dir" 0 root "$@"; }

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
    server_tar "$d/cairnd_${V}_${p}.tar.gz" "$d/src" cairnd LICENSE
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
server_tar "$d/cairnd_${V}_darwin_arm64.tar.gz" "$d/src" cairnd cairn LICENSE
expect_fail "the server archive holding cairn fails" "$d" "must hold exactly 'cairnd' and 'LICENSE'"

d="$(fresh zip-carries-server)"
make_zip "$d/cairn_${V}_windows_amd64.zip" "$d/src" cairn.exe cairnd LICENSE
expect_fail "a windows CLI zip holding cairnd fails" "$d" "must hold exactly 'cairn.exe' and 'LICENSE'"

d="$(fresh no-license)"
server_tar "$d/cairnd_${V}_linux_arm64.tar.gz" "$d/src" cairnd
expect_fail "an archive without LICENSE fails" "$d" "must hold exactly 'cairnd' and 'LICENSE'"

d="$(fresh missing-platform)"
rm "$d/cairnd_${V}_darwin_amd64.tar.gz"; write_sums "$d"
expect_fail "a missing cairnd platform fails" "$d" "no cairnd archive for darwin_amd64"

d="$(fresh server-is-really-cli)"
cp "$d/src/cairn" "$d/src/cairnd"
server_tar "$d/cairnd_${V}_linux_amd64.tar.gz" "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "a cairnd without server code fails the positive control" "$d" "positive control failed"

d="$(fresh server-with-cli-code)"
printf 'x %s/internal/httpapi %s/internal/clicmd\n' "$MODULE" "$MODULE" > "$d/src/cairnd"
server_tar "$d/cairnd_${V}_linux_amd64.tar.gz" "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "a cairnd carrying CLI code fails" "$d" "CLI CODE IN THE SERVER"

d="$(fresh builder-owned-server)"
make_tar "$d/cairnd_${V}_linux_arm64.tar.gz" "$d/src" 501 joestump cairnd LICENSE; write_sums "$d"
expect_fail "a cairnd archive carrying the builder's owner fails" "$d" "NOT REPRODUCIBLE"

d="$(fresh wrong-mode-server)"
chmod 600 "$d/src/LICENSE"
python3 - "$d/cairnd_${V}_darwin_amd64.tar.gz" "$d/src" <<'PY'
import os, sys, tarfile
out, d = sys.argv[1], sys.argv[2]
with tarfile.open(out, "w:gz") as t:
    for n, mode in (("cairnd", 0o755), ("LICENSE", 0o600)):
        ti = t.gettarinfo(os.path.join(d, n), n)
        ti.uid = ti.gid = 0; ti.uname = ti.gname = "root"; ti.mode = mode
        with open(os.path.join(d, n), "rb") as f:
            t.addfile(ti, f)
PY
write_sums "$d"
expect_fail "a cairnd archive with LICENSE's on-disk mode fails" "$d" "NOT REPRODUCIBLE"

d="$(fresh unexpected-archive)"
server_tar "$d/cairnd_${V}_windows_amd64.tar.gz" "$d/src" cairnd LICENSE; write_sums "$d"
expect_fail "an archive the release does not account for fails" "$d" "an archive exists that this release does not account for"

d="$(fresh unlisted-checksum)"
grep -v "cairn_${V}_linux_arm64" "$d/checksums.txt" > "$d/c" && mv "$d/c" "$d/checksums.txt"
expect_fail "an archive missing from checksums.txt fails" "$d" "checksums.txt does not list"

mkdir -p "$work/empty"
expect_fail "an empty dist fails" "$work/empty" "no cairn archive for linux_amd64"
expect_fail "a missing dist fails" "$work/does-not-exist" "dist directory not found"

echo "$pass passed, $failures failed"
[ "$failures" -eq 0 ]
