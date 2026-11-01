---
status: proposed
date: 2026-11-01
decision-makers: [joestump]
---

# ADR-0035: Automated Git Writes Authenticate Through A Credential Helper

## Context and Problem Statement

Cairn's release pipeline writes to a repository other than the one the run
belongs to: the `tap-bump` job pushes the rendered formula to
`stump.wtf/homebrew-tap`. That job died on v0.7.0 with

    fatal: could not read Username for 'https://forge.internal': No such
    device or address
    exitcode '128': failure

(#489; the hostname is elided here because the published docs may not carry
it), which named no secret and no fix, and left the formula to be bumped by
hand. Nothing else in the message says "credential", and an Actions container
has no terminal to answer the question git was asking.

The job did have a token. It was in the job's environment and attached to the
push as `http.extraheader="Authorization: token …"`. The server refused that
header, and because an extra header is not a *credential* in git's model, git
did not treat the request as an attempted authentication: it went looking for a
username from somewhere else, found a container with no tty, and died in a way
that reads like a broken checkout rather than a rejected secret.

The thread does not decide how an automated git write should authenticate here,
and the finding suggested two different directions (a push-capable token through
whatever plumbing the other jobs use, or dropping `git push` for the contents
API). This decides it, because the next repo that needs to write somewhere else
will otherwise re-derive it.

## Decision Drivers

* The failure must name itself. A release job that dies with git's prompt error
  costs a human a morning; #489 did.
* A credential that is present but wrong must not make the run worse than no
  credential at all. Reads that need no authentication should keep working.
* Nothing a run holds may reach `ps`, a step log, `.git/config`, or a URL git
  prints back verbatim in `fatal: Authentication failed for '<url>'`.
* The pattern should be reusable rather than another block of YAML nobody
  copies correctly twice.
* Terminals do not exist in a runner, so anything that can fall back to one has
  to close that door explicitly.

## Considered Options

1. **`http.extraheader="Authorization: token $TOKEN"` on every git call.** The
   form the failing push used. A header is sent on every request whether or not
   the server asked for it, so a refused token breaks calls that needed no
   credential: `git ls-remote` on the tap with no credential at all returns the
   refs, and the same command with a refused header returns
   `remote: Failed to authenticate user`. It also sits outside git's credential
   state machine, which is how a job with a token in hand still ended up asking
   a terminal for a username.
2. **Token in the remote URL (the userinfo form: credentials between the
   scheme and the host, before the at-sign).** Works,
   and `stumpcloud/ansible` uses it for its own checkouts. But the token is then
   committed to `.git/config` as the remote URL, shows in `git remote -v`, and
   git quotes that URL back in its own failure output, so the log that reports
   the failure also carries the secret.
3. **Commit through the contents API instead of `git push`.** What the tap's own
   bump workflow does. It gives up the render-diff-commit shape this job already
   has, and the no-op path ("already at this version, nothing to push") that
   keeps a re-run of a tag from opening a duplicate commit, for a path that only
   one file needed anyway.
4. **A per-invocation credential helper (chosen).** `git -c credential.helper=`
   followed by `git -c credential.helper='!f() { … }; f'`, with
   `GIT_TERMINAL_PROMPT=0` so asking is impossible. The helper is never written
   to `.git/config`, is invoked only when the server actually challenges
   (verified: the same command with a helper holding a useless token still
   clones an anonymously readable repo), and carries the token in git's own
   Basic-auth path, where the password is the credential that decides identity.

## Decision

Use option 4, wrapped in `scripts/git-with-token.sh`, and have every git call a
Cairn workflow makes against another repository go through it. The script takes
the *name* of the variable holding the token, never the token, so no call site
puts a secret in a command line; it exports that variable so the helper's shell
can read it; it passes git's exit status through so callers' retry logic is
unchanged; and when git fails in a way that means "your credential did not
work", it adds one `::error::` line naming the secret to replace.

Reads stay as they were: the helper does not send a credential unless asked, so
the clone of the anonymously readable tap costs nothing extra and still works if
the token dies.

## Consequences

* A dead, revoked, or under-scoped `TAP_PUSH_TOKEN` no longer looks like a
  broken checkout. It also does not become fixable by this repo: the job says
  which secret is at fault, and rotating it is an operator action, not a
  pipeline change.
* The `tap-bump` job's shape is unchanged: render from
  `packaging/homebrew/cairn.rb.tmpl`, commit only when the rendered formula
  differs, and no-op cleanly when the tap already pins this version.
* `scripts/git-with-token.sh` is exercised by `scripts/git-with-token.test.sh`
  under `make test-scripts`, including the quoting of the helper string, which
  is the part that fails silently.
* If a second repo needs the same write path, the helper and its message come
  with it; the alternative is a second YAML block with a second way to get it
  wrong.

## More Information

`docs/adrs/ADR-0034-the-metrics-scrape-credential-is-a-resolved-static-token.md`
settles which credential family reaches a server endpoint; this one is about the
transport a runner uses to hand a credential to git, and the two do not overlap.
