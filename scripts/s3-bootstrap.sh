#!/usr/bin/env bash
#
# Bootstrap the Garage object store that docker-compose.yml (and the self-hosting
# guide's compose file) starts, so cairn finds a bucket and a working key the
# first time it connects.
#
# Garage is not MinIO: it refuses to serve S3 at all until a node role is assigned
# and that layout is applied, and S3 credentials exist only once a key has been
# imported and allowed on a bucket. That is four admin commands. They run against
# the running container because the Garage image is distroless: there is no shell
# in it, so nothing can be scripted inside it, and CI's integration job drives it
# the same way.
#
# Every step treats "already done" as success: `make up` runs this on every start,
# and a store that comes back from a retained volume already has its layout,
# bucket and key. Only a store that never answers is an error.
#
# Credentials: Garage wants a key id of GK plus 12 hex bytes and a secret of 32
# hex bytes, so a pair cannot be typed casually. With CAIRN_S3_ACCESS_KEY and
# CAIRN_S3_SECRET_KEY unset (the local development case) this script generates a
# pair once and keeps it in $S3_CREDS_FILE, gitignored, which is also what makes
# `make test` exercise the real S3 code paths locally. An operator who sets both
# variables gets exactly those credentials instead, and nothing is written.
#
# @joestump-agent 10/19/2026 - Added for cairn#388.

set -euo pipefail

# `docker compose` by default; COMPOSE_CMD lets a caller point this at a compose
# file elsewhere (`COMPOSE_CMD="docker compose -f /srv/cairn/compose.yaml"`)
# without editing the script.
read -r -a compose_cmd <<< "${COMPOSE_CMD:-docker compose}"
garage_service="${GARAGE_SERVICE:-garage}"
bucket="${CAIRN_S3_BUCKET:-cairn}"
zone="${GARAGE_ZONE:-dc1}"
capacity="${GARAGE_CAPACITY:-10G}"
wait_attempts="${GARAGE_WAIT_ATTEMPTS:-60}"
creds_file="${S3_CREDS_FILE:-garage/dev-credentials.env}"
access_key="${CAIRN_S3_ACCESS_KEY:-}"
secret_key="${CAIRN_S3_SECRET_KEY:-}"

fail() { echo "s3-bootstrap: $1" >&2; exit 1; }

randhex() {
  openssl rand -hex "$1" 2>/dev/null && return 0
  head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
}

# The admin CLI runs inside the container, where /etc/garage.toml is.
admin() { "${compose_cmd[@]}" exec -T "$garage_service" /garage "$@"; }

# Checked before anything is touched, so a key generated the obvious way
# (`openssl rand -hex 16`) is not discovered to be unusable after the layout work
# is already done.
check_shapes() {
  printf '%s' "$access_key" | grep -Eq '^GK[0-9a-fA-F]{24}$' ||
    fail "access key '${access_key}' is not a Garage key id: GK followed by 12 hex bytes, e.g. GK\$(openssl rand -hex 12)"
  printf '%s' "$secret_key" | grep -Eq '^[0-9a-fA-F]{64}$' ||
    fail "secret access key is not a Garage secret: 32 hex bytes, e.g. \$(openssl rand -hex 32)"
}

if [ -n "$access_key" ] && [ -z "$secret_key" ]; then
  fail "CAIRN_S3_ACCESS_KEY is set without CAIRN_S3_SECRET_KEY; Garage needs the pair"
fi
if [ -z "$access_key" ] && [ -n "$secret_key" ]; then
  fail "CAIRN_S3_SECRET_KEY is set without CAIRN_S3_ACCESS_KEY; Garage needs the pair"
fi

generated=""
if [ -z "$access_key" ]; then
  if [ -f "$creds_file" ]; then
    # Reuse the pair from the first run: the bucket's permissions already name
    # it, and a fresh one every start would leave cairn pointing at a key the
    # store has never heard of.
    access_key="$(sed -n 's/^CAIRN_S3_ACCESS_KEY=//p' "$creds_file" | head -n 1)"
    secret_key="$(sed -n 's/^CAIRN_S3_SECRET_KEY=//p' "$creds_file" | head -n 1)"
  else
    access_key="GK$(randhex 12)"
    secret_key="$(randhex 32)"
    generated=yes
  fi
fi
check_shapes

ready=""
for _ in $(seq 1 "$wait_attempts"); do
  if admin status >/dev/null 2>&1; then ready=yes; break; fi
  sleep 1
done
[ -n "$ready" ] || fail "garage did not answer 'garage status' within ${wait_attempts}s; is the ${garage_service} service up (docker compose ps)?"

# A single-node cluster still needs a role before it will serve anything. The
# marker is what an unassigned node prints, and CI's bootstrap keys off the same
# string, so a cluster that is already configured is skipped instead of staged
# into a new layout version on every start.
if admin status 2>/dev/null | grep -q 'NO ROLE ASSIGNED'; then
  node="$(admin node id | head -n 1 | cut -d@ -f1)"
  [ -n "$node" ] || fail "'garage node id' printed no node id"
  admin layout assign -z "$zone" -c "$capacity" "$node" >/dev/null 2>&1 ||
    fail "could not assign a ${zone}/${capacity} role to node ${node:0:16}"
  # apply wants the version number that assign just staged, which is one past the
  # applied layout; reading it beats assuming a fresh cluster is at version 1.
  applied="$(admin layout show 2>/dev/null |
    sed -n 's/^Current cluster layout version: *\([0-9][0-9]*\).*/\1/p' | head -n 1)"
  admin layout apply --version "$(( ${applied:-0} + 1 ))" >/dev/null 2>&1 ||
    fail "could not apply the staged layout"
fi

# cairn creates the bucket on first connect too, so a create that fails is only
# fatal when the bucket is genuinely absent: checking here turns a broken store
# into this script's error rather than a line in cairn's log.
admin bucket create "$bucket" >/dev/null 2>&1 ||
  admin bucket info "$bucket" >/dev/null 2>&1 ||
  fail "bucket ${bucket} could not be created and does not exist"

# Import, not create: create mints random credentials this script would then have
# to scrape back out of stdout, and the S3 env already names the pair. A failed
# import that finds the key present means a previous run did this.
key_errors="$(admin key import --yes -n cairn "$access_key" "$secret_key" 2>&1 >/dev/null)" || {
  admin key info "$access_key" >/dev/null 2>&1 ||
    fail "could not import access key ${access_key}: ${key_errors}"
}

admin bucket allow --read --write --owner "$bucket" --key "$access_key" >/dev/null 2>&1 ||
  fail "could not grant ${access_key} read/write on ${bucket}"

if [ -n "$generated" ]; then
  mkdir -p "$(dirname "$creds_file")"
  # Sourced with `set -a`, so it carries both name families: CAIRN_S3_* for a
  # server run from the host, CAIRN_TEST_S3_* for the suites that otherwise
  # self-skip and report a green run without ever touching an object store.
  umask 077
  {
    echo "# Generated by scripts/s3-bootstrap.sh: local development credentials"
    echo "# for this machine's Garage. Safe to delete; the next run regenerates it."
    echo "CAIRN_S3_ENDPOINT=localhost:9000"
    echo "CAIRN_S3_BUCKET=${bucket}"
    echo "CAIRN_S3_ACCESS_KEY=${access_key}"
    echo "CAIRN_S3_SECRET_KEY=${secret_key}"
    echo "CAIRN_TEST_S3_ENDPOINT=localhost:9000"
    echo "CAIRN_TEST_S3_BUCKET=${bucket}"
    echo "CAIRN_TEST_S3_ACCESS_KEY=${access_key}"
    echo "CAIRN_TEST_S3_SECRET_KEY=${secret_key}"
  } > "$creds_file"
  echo "s3-bootstrap: generated a development key pair in ${creds_file}"
fi

echo "s3-bootstrap: bucket '${bucket}' ready; key ${access_key} has read/write on it."
echo "  endpoint in the compose network: ${garage_service}:3900"
echo "  endpoint on this machine:        localhost:9000 (published by docker-compose.yml)"
if [ -n "$generated" ]; then
  echo "  to point cairn at it:            set -a; . ${creds_file}; set +a"
fi
