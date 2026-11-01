#!/usr/bin/env bash
#
# Tests For git-with-token.sh
#
# The script exists because a real failure looked like nothing: git's "could
# not read Username … No such device or address" (#489) named neither the
# secret nor the fix. So each case here has to fail for its own reason and say
# the right thing while doing it — matching the message, not just the exit
# code, is the point.
#
# Two cases carry the weight:
#
#   * the helper git is handed must really answer with Basic credentials, which
#     is checked by running the helper the way git does (the stub below runs
#     it), because the quoting around the token's variable name is easy to get
#     wrong silently;
#   * the token's value must never reach argv or the output, since both end up
#     in a step log.
#
# A stub `git` on PATH stands in for the real one: no network, no credential,
# and every invocation recorded so the assertions read what was actually
# passed rather than what the script meant to pass.
#
# @joestump-agent 11/01/2026 - Written with git-with-token.sh for #489.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/git-with-token.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin"
cat > "$work/bin/git" <<'STUB'
#!/usr/bin/env bash
# Records argv and the no-prompt environment, runs any credential helper it was
# handed the way git would, then behaves as STUB_GIT_RC / STUB_GIT_STD* say.
printf '%s\n' "$@" > "$STUB_ARGV"
env | grep -E '^(GIT_TERMINAL_PROMPT|GIT_ASKPASS)=' | sort > "$STUB_ENV"
for arg in "$@"; do
  case "$arg" in
    credential.helper='!'*)
      body="${arg#credential.helper=!}"
      sh -c "$body get" > "$STUB_HELPER_OUT" 2> "$STUB_HELPER_ERR"
      ;;
  esac
done
[ -z "${STUB_GIT_STDERR:-}" ] || printf '%s\n' "$STUB_GIT_STDERR" >&2
[ -z "${STUB_GIT_STDOUT:-}" ] || printf '%s\n' "$STUB_GIT_STDOUT"
exit "${STUB_GIT_RC:-0}"
STUB
chmod +x "$work/bin/git"
export PATH="$work/bin:$PATH"

pass=0
failures=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1"; failures=$((failures + 1)); }

# run <expected-exit> <git-args...> invokes the script with the stub wired up
# and the STUB_GIT_* knobs exported for exactly one call. Git's knobs are set as
# ordinary assignments before each call, since an assignment prefixed on a
# function call never reaches the stub process.
run() {
  local want="$1"; shift
  local rc=0
  : > "$work/argv"; : > "$work/env"; : > "$work/helper"; : > "$work/helper_err"
  export STUB_ARGV="$work/argv" STUB_ENV="$work/env"
  export STUB_HELPER_OUT="$work/helper" STUB_HELPER_ERR="$work/helper_err"
  export STUB_GIT_RC="${STUB_GIT_RC:-0}"
  if [ -n "${STUB_GIT_STDERR:-}" ]; then export STUB_GIT_STDERR; else unset STUB_GIT_STDERR; fi
  if [ -n "${STUB_GIT_STDOUT:-}" ]; then export STUB_GIT_STDOUT; else unset STUB_GIT_STDOUT; fi
  out="$("$script" "$@" 2>&1)" || rc=$?
  unset STUB_GIT_STDERR STUB_GIT_STDOUT
  status="$rc"
  [ "$rc" -eq "$want" ] || bad "$name: exited $rc, want $want: $out"
}

unset TAP_PUSH_TOKEN
name="an unset token stops the command before git runs"
run 1 TAP_PUSH_TOKEN ls-remote https://example.invalid/tap.git
if printf '%s' "$out" | grep -q '::error::' &&
   printf '%s' "$out" | grep -q 'TAP_PUSH_TOKEN is unset or empty' &&
   [ ! -s "$work/argv" ]; then ok "$name"; else bad "$name: $out"; fi

name="a set-but-empty token is refused the same way"
export TAP_PUSH_TOKEN=''
run 1 TAP_PUSH_TOKEN push -q origin HEAD:main
if printf '%s' "$out" | grep -q 'TAP_PUSH_TOKEN is unset or empty' &&
   [ ! -s "$work/argv" ]; then ok "$name"; else bad "$name: $out"; fi

name="a name that is not a variable name is refused"
export TAP_PUSH_TOKEN='sekrit-token-value'
run 1 'a-broken;name' ls-remote https://example.invalid/tap.git
if printf '%s' "$out" | grep -q 'not an environment variable name' &&
   [ ! -s "$work/argv" ]; then ok "$name"; else bad "$name: $out"; fi

name="no git arguments is refused"
run 1 TAP_PUSH_TOKEN
if printf '%s' "$out" | grep -q 'usage:'; then ok "$name"; else bad "$name: $out"; fi

# The good path: git is called with prompts made impossible, its stdout reaches
# the caller untouched, and nothing in what was passed carries the secret.
export TAP_PUSH_TOKEN='sekrit-token-value'
name="a set token runs git with a credential helper and no terminal"
STUB_GIT_STDOUT='abc123	refs/heads/main'
run 0 TAP_PUSH_TOKEN ls-remote https://example.invalid/tap.git
if grep -qx 'ls-remote' "$work/argv" &&
   grep -q 'refs/heads/main' <<< "$out" &&
   grep -q 'GIT_TERMINAL_PROMPT=0' "$work/env" &&
   grep -qx 'credential.helper=' "$work/argv" &&
   ! grep -q 'sekrit-token-value' "$work/argv"; then
  ok "$name"
else
  bad "$name: $out | $(tr '\n' ' ' < "$work/argv")"
fi

name="the helper git is handed answers with Basic credentials"
run 0 TAP_PUSH_TOKEN -- clone https://example.invalid/tap.git tap
if grep -qx 'username=x-access-token' "$work/helper" &&
   grep -qx 'password=sekrit-token-value' "$work/helper"; then
  ok "$name"
else
  bad "$name: helper said $(tr '\n' ' ' < "$work/helper")$(cat "$work/helper_err")"
fi

name="a -- separator is consumed and never reaches git"
run 0 TAP_PUSH_TOKEN -- ls-remote https://example.invalid/tap.git
if grep -qx -- '--' "$work/argv"; then bad "$name: '--' was passed to git"; else ok "$name"; fi

name="git's prompt error becomes an error naming the secret"
STUB_GIT_RC=128
STUB_GIT_STDERR="fatal: could not read Username for 'https://gitea.stump.rocks': No such device or address"
run 128 TAP_PUSH_TOKEN push -q origin HEAD:main
if printf '%s' "$out" | grep -q 'No such device or address' &&
   printf '%s' "$out" | grep -q '::error::' &&
   printf '%s' "$out" | grep -q 'TAP_PUSH_TOKEN is set but the server refused it' &&
   ! grep -q 'sekrit-token-value' <<< "$out"; then
  ok "$name"
else
  bad "$name: $out"
fi

name="a refused credential is reported the same way"
STUB_GIT_RC=128
STUB_GIT_STDERR="remote: Failed to authenticate user
fatal: Authentication failed for 'https://gitea.stump.rocks/stump.wtf/homebrew-tap.git/'"
run 128 TAP_PUSH_TOKEN push -q origin HEAD:main
if printf '%s' "$out" | grep -q 'TAP_PUSH_TOKEN is set but the server refused it'; then
  ok "$name"
else
  bad "$name: $out"
fi

name="an unrelated git failure is not blamed on the token"
STUB_GIT_RC=128
STUB_GIT_STDERR="fatal: not a git repository: '.git'"
run 128 TAP_PUSH_TOKEN status
if printf '%s' "$out" | grep -q 'not a git repository' &&
   ! printf '%s' "$out" | grep -q '::error::'; then
  ok "$name"
else
  bad "$name: $out"
fi

echo "$pass passed, $failures failed"
[ "$failures" -eq 0 ]
