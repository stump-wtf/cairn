#!/usr/bin/env bash
#
# Run Git With A Token, In A Container With No Terminal
#
# Actions containers have no terminal. The moment git decides it needs a
# username it dies with
#
#   fatal: could not read Username for 'https://gitea.stump.rocks': No such
#   device or address
#
# — exit 128, and nothing in it saying that a credential is what went missing.
# That is how cairn v0.7.0's tap bump failed (#489): the step carried its token
# in `http.extraheader`, the server refused the header, and because an extra
# header is not a *credential* as far as git is concerned, git fell back to
# asking a terminal that does not exist. The bump landed in the tap by hand.
#
# So credentials go through git's credential path instead: a helper named per
# invocation (never written to .git/config), GIT_TERMINAL_PROMPT=0 so asking is
# impossible, and Basic auth, whose username Gitea ignores when the password is
# a PAT. Three things follow from that choice, and they are the reason it is
# better than the header it replaces (ADR-0035):
#
#   * A helper speaks only when the server asks. An anonymously readable repo
#     still clones anonymously even with a dead token, so a bad token fails at
#     the one command that needs write access instead of poisoning every read
#     in front of it. A header is sent on every request, good or not.
#   * The token stays out of argv, out of the remote URL and out of .git/config,
#     so it cannot surface in `ps`, in a `git remote -v`, or in git's own
#     `fatal: Authentication failed for '<url>'` line, which quotes the URL.
#   * When it does fail, this script says which secret to go and fix, which is
#     the part git never says.
#
# The token is named, never passed: an argument would land in argv and in the
# step log that echoes the command line.
#
# Usage:
#   scripts/git-with-token.sh <token-env-var> [--] <git-args...>
#
# Exit status is git's, so a caller's `if`/`||` and retry logic keep working.
#
# @joestump-agent 11/01/2026 - Created for #489, which died on git's prompt
# error with a token sitting unused in the job's environment.

set -euo pipefail

fail() { echo "::error::$*" >&2; exit 1; }

[ "$#" -ge 2 ] || fail "usage: $0 <token-env-var> [--] <git-args...>"

token_var="$1"
shift
[ "${1:-}" = "--" ] && shift
[ "$#" -ge 1 ] || fail "no git arguments given after $token_var"

# The name goes into a command string git hands to a shell, so it stays on the
# small set a shell would treat as one word and nothing more.
case "$token_var" in
  [A-Za-z_][A-Za-z0-9_]*) ;;
  *) fail "'$token_var' is not an environment variable name" ;;
esac

[ -n "${!token_var:-}" ] ||
  fail "$token_var is unset or empty, so there is no credential to offer. Mint a Gitea PAT with write access to the repository this job pushes to and add it as an Actions secret named $token_var."

# The helper runs in a shell git starts, which sees this process's exported
# environment. Without the export it would read an empty variable and the
# command would fail as though the secret were wrong.
export "$token_var"

err="$(mktemp)"
trap 'rm -f "$err"' EXIT

rc=0
GIT_TERMINAL_PROMPT=0 GIT_ASKPASS= git \
  -c credential.helper= \
  -c "credential.helper=!f() { echo username=x-access-token; echo \"password=\${$token_var}\"; }; f" \
  "$@" 2>"$err" || rc=$?

cat "$err" >&2

if [ "$rc" -ne 0 ]; then
  # git's own wording varies by version and by whether it was refused or never
  # got to ask, so all of them are matched here: the one thing a reader needs
  # is to be pointed at the secret rather than at git.
  if grep -Eqi 'could not read Username|terminal prompts disabled|Authentication failed|Failed to authenticate user|error: 40[13]|invalid (username|password|credentials)|permission denied|access denied' "$err"; then
    echo "::error::$token_var is set but the server refused it: expired, revoked, scoped below what this command needs, or owned by an account without write access. Replace the Actions secret named $token_var and re-run." >&2
  fi
  exit "$rc"
fi
