#!/usr/bin/env bash
#
# Tests For s3-bootstrap.sh
#
# The script's job is to leave a Garage cluster serving S3 for cairn, and to be
# boring when it runs a second time (`make up` calls it on every start), so a step
# that fails because the work is already done would break every existing checkout.
# Docker is replaced by a stub that answers from files in a scratch directory, so
# the cases are about the script's decisions (assign or not, which layout version
# to apply, generate or reuse a key, tolerate or fail) rather than about Garage.
#
# @joestump-agent 10/19/2026 - Added for cairn#388.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
bootstrap="$here/s3-bootstrap.sh"

pass=0
failures=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1"; failures=$((failures + 1)); }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
creds="$work/dev-credentials.env"

# A fresh cluster: one node with no role, no bucket, no key.
new_stub() {
  stub="$work/stub"
  rm -rf "$stub"; mkdir -p "$stub/bin" "$stub/state"
  : > "$stub/state/log"
  printf 'NO ROLE ASSIGNED\n' > "$stub/state/status"
  printf '0\n' > "$stub/state/layout_version"
  : > "$stub/state/buckets"
  : > "$stub/state/keys"
  cat > "$stub/bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
state="$(dirname "$0")/../state"
# Drop "compose" and the exec plumbing, keep the garage command line.
args=("$@")
while [ "${args[0]:-}" != "exec" ] && [ "${#args[@]}" -gt 0 ]; do args=("${args[@]:1}"); done
# Drop "exec", "-T", the service name and the /garage binary itself.
args=("${args[@]:4}")
echo "${args[*]}" >> "$state/log"
case "${args[0]:-} ${args[1]:-}" in
  "status "*)
    if [ -e "$state/fail_status" ]; then exit 1; fi
    cat "$state/status" ;;
  "node id")    printf 'abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234@garage:3901\n' ;;
  "layout show")
    printf 'Current cluster layout version: %s\n' "$(cat "$state/layout_version")" ;;
  "layout assign")
    printf 'Current cluster layout version: %s\n' "$(cat "$state/layout_version")" ;;
  "layout apply")
    v="${args[3]:-}"; [ -n "$v" ] || exit 1
    printf '%s\n' "$v" > "$state/layout_version"
    printf '8c8a  dc1  10 GB\n' > "$state/status" ;;
  "bucket create")
    if [ -e "$state/fail_bucket_create" ]; then exit 1; fi
    printf '%s\n' "${args[2]}" >> "$state/buckets" ;;
  "bucket info")
    grep -qx "${args[2]}" "$state/buckets" ;;
  "key import")
    if [ -e "$state/fail_key_import" ]; then exit 1; fi
    printf '%s\n' "${args[5]}" >> "$state/keys" ;;
  "key info")
    grep -qx "${args[2]}" "$state/keys" ;;
  "bucket allow")
    if [ -e "$state/fail_bucket_allow" ]; then exit 1; fi ;;
  *) exit 0 ;;
esac
STUB
  chmod +x "$stub/bin/docker"
}

# Credentials set explicitly: the self-host case, where the operator's .env names
# the pair the whole stack must use.
run_stub() {
  PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY="${TEST_ACCESS_KEY:-GK000000000000000000000001}" \
    CAIRN_S3_SECRET_KEY="${TEST_SECRET_KEY:-$(printf 'a%.0s' $(seq 64))}" \
    "$@" "$bootstrap"
}

# No credentials set: the local development case, which generates a pair.
run_generated() {
  PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY= CAIRN_S3_SECRET_KEY= "$@" "$bootstrap"
}

expect_fail() {
  local name="$1" want="$2"; shift 2
  local out
  if out="$("$@" 2>&1)"; then
    bad "$name: expected failure, got success"
  elif printf '%s' "$out" | grep -q -F "$want"; then
    ok "$name"
  else
    bad "$name: failed, but not with \"$want\": $out"
  fi
}

# 1. A fresh cluster is brought up in one pass, and the layout applied is the one
#    assign just staged (version 0 applied, so apply 1), not a guessed constant.
new_stub
out="$(run_stub)"
grep -q 'layout assign -z dc1 -c 10G abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234' "$work/stub/state/log" \
  && ok "fresh: assigns a role to the node it discovered" \
  || { bad "fresh: no role assigned; log: $(cat "$work/stub/state/log")"; }
[ "$(cat "$work/stub/state/layout_version")" = "1" ] \
  && ok "fresh: applies layout version 1" \
  || bad "fresh: applied version $(cat "$work/stub/state/layout_version"), want 1"
[ "$(cat "$work/stub/state/buckets")" = "cairn" ] \
  && ok "fresh: creates the cairn bucket" || bad "fresh: bucket missing"
[ "$(cat "$work/stub/state/keys")" = "GK000000000000000000000001" ] \
  && ok "fresh: imports the key the environment named" || bad "fresh: key missing"
printf '%s' "$out" | grep -q "$(printf 'a%.0s' $(seq 64))" \
  && bad "fresh: printed the secret access key" || ok "fresh: never prints the secret"

# 2. The second start must not stage a new layout version: re-running against a
#    configured cluster leaves the applied version alone.
before="$(cat "$work/stub/state/layout_version")"
: > "$work/stub/state/log"
run_stub >/dev/null
[ "$(cat "$work/stub/state/layout_version")" = "$before" ] \
  && ok "rerun: leaves the applied layout version alone" \
  || bad "rerun: bumped the layout version to $(cat "$work/stub/state/layout_version")"
grep -q 'bucket allow' "$work/stub/state/log" \
  && ok "rerun: still grants the key its permissions" || bad "rerun: skipped bucket allow"

# 3. An interrupted run can leave a role staged but unapplied; the version to
#    apply is read from the cluster, so this converges instead of erroring.
new_stub
printf '3\n' > "$work/stub/state/layout_version"
run_stub >/dev/null
[ "$(cat "$work/stub/state/layout_version")" = "4" ] \
  && ok "staged: applies the version past the applied one" \
  || bad "staged: applied $(cat "$work/stub/state/layout_version"), want 4"

# 4. Bucket and key that already exist are successes, not errors: create and
#    import are the steps that complain loudest on a restarted stack.
new_stub
printf 'cairn\n' > "$work/stub/state/buckets"
printf 'GK000000000000000000000001\n' > "$work/stub/state/keys"
touch "$work/stub/state/fail_bucket_create" "$work/stub/state/fail_key_import"
if run_stub >/dev/null; then ok "existing: tolerates a bucket and key from a previous run"
else bad "existing: failed on a store that is already configured"; fi

# 5. A store that never answers is the one thing that must stop `make up`, and it
#    must name the check to run rather than exit 1 silently.
new_stub
touch "$work/stub/state/fail_status"
expect_fail "unreachable: stops with the diagnosis" \
  "did not answer" \
  env PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY=GK000000000000000000000001 \
    CAIRN_S3_SECRET_KEY="$(printf 'a%.0s' $(seq 64))" "$bootstrap"

# 6. A bucket that cannot be created and does not exist is fatal: cairn would
#    otherwise fail later, somewhere in its own log.
new_stub
touch "$work/stub/state/fail_bucket_create"
expect_fail "missing bucket: stops rather than deferring to cairn" \
  "could not be created" \
  env PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY=GK000000000000000000000001 \
    CAIRN_S3_SECRET_KEY="$(printf 'a%.0s' $(seq 64))" "$bootstrap"

# 7. Granting the key is the last thing standing between a green bootstrap and a
#    cairn that fails on its first upload, so a refusal there is an error.
new_stub
touch "$work/stub/state/fail_bucket_allow"
expect_fail "allow refused: stops with the grant that failed" \
  "could not grant" \
  env PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY=GK000000000000000000000001 \
    CAIRN_S3_SECRET_KEY="$(printf 'a%.0s' $(seq 64))" "$bootstrap"

# 8. A key generated the obvious way (`openssl rand -hex 16`) is not a Garage key
#    id. Garage would reject it after the layout work, so it is rejected here,
#    with the shape spelled out and before the cluster is touched.
new_stub
expect_fail "key shape: rejects a non-Garage access key id" \
  "GK followed by 12 hex bytes" \
  env PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY="$(printf 'a%.0s' $(seq 32))" \
    CAIRN_S3_SECRET_KEY="$(printf 'a%.0s' $(seq 64))" "$bootstrap"
[ ! -s "$work/stub/state/log" ] \
  && ok "key shape: rejects before touching the cluster" \
  || bad "key shape: ran cluster commands with an invalid key id"

# 9. Half a pair is its own mistake: with only the id set, the generated secret
#    would silently win and the operator's .env would be a lie.
new_stub
expect_fail "half a pair: refuses to guess the missing half" \
  "without CAIRN_S3_SECRET_KEY" \
  env PATH="$work/stub/bin:$PATH" COMPOSE_CMD="$work/stub/bin/docker compose" \
    GARAGE_WAIT_ATTEMPTS=1 S3_CREDS_FILE="$creds" \
    CAIRN_S3_ACCESS_KEY=GK000000000000000000000001 CAIRN_S3_SECRET_KEY= "$bootstrap"

# 10. With nothing set, a pair is generated, written where cairn and the test
#     suites can source it, and imported in the shape Garage accepts.
new_stub
rm -f "$creds"
run_generated >/dev/null
[ -f "$creds" ] && ok "generated: leaves a credentials file behind" || bad "generated: no credentials file"
gen_key="$(sed -n 's/^CAIRN_S3_ACCESS_KEY=//p' "$creds" | head -n 1)"
gen_secret="$(sed -n 's/^CAIRN_S3_SECRET_KEY=//p' "$creds" | head -n 1)"
printf '%s' "$gen_key" | grep -Eq '^GK[0-9a-f]{24}$' \
  && ok "generated: writes a Garage key id" || bad "generated: bad key id '$gen_key'"
printf '%s' "$gen_secret" | grep -Eq '^[0-9a-f]{64}$' \
  && ok "generated: writes a Garage secret" || bad "generated: bad secret length"
[ "$(cat "$work/stub/state/keys")" = "$gen_key" ] \
  && ok "generated: imports the pair it wrote" || bad "generated: imported a different key"
grep -q '^CAIRN_TEST_S3_ACCESS_KEY='"$gen_key" "$creds" \
  && ok "generated: names the key for the test suites too" || bad "generated: CAIRN_TEST_S3_* missing"

# 11. The next start reuses that pair: the bucket's permissions already name it,
#     and a new key every start would leave cairn holding one Garage never saw.
new_stub
run_generated >/dev/null
[ "$(cat "$work/stub/state/keys")" = "$gen_key" ] \
  && ok "generated: reuses the pair on the next run" \
  || bad "generated: minted a new key ($(cat "$work/stub/state/keys")) on the second run"

echo
echo "s3-bootstrap: $pass passed, $failures failed"
[ "$failures" -eq 0 ]
