---
status: proposed
date: 2026-11-05
decision-makers: [joestump]
extends: []
related: [ADR-0004, ADR-0005, ADR-0025]
---

# ADR-0035: A Comment Body Naming a File Is Read — One Path Convention Across the CLI

## Context and Problem Statement

Almost every positional argument in this CLI that can name a file reads the file:
bare `cairn <path>` and `cairn add <file>...` open the path and share what is
inside it ("Pipe and Path Ingest", SPEC-0008). `cairn comment <id> [body]` is the
one exception — its second positional is stored verbatim ("Annotation Write",
SPEC-0008). That asymmetry is a trap for exactly the caller that posts the most
comments: an unattended one-shot that writes its report to a scratch file and then
passes the path, because everywhere else in the tool that is how a file is passed.

Two confirmed instances, both from harness#1048:

* 2026-10-25T18:51Z, the handoff for stumpcloud/stumpcloud#643: its only comment is
  `/dev/shm/tmp.VOJ8oK6Yfy/comment.md`.
* 2026-11-05T04:09Z, the handoff for stump.wtf/mixtape#38: its only comment is
  `/tmp/cairncomment.md`.

The comment is the report's only home. The project manager reads it to decide
re-kick, park, or landed, so a body that is a path on someone else's machine is not
a cosmetic defect: the first instance above funded a duplicate dispatch, and the
second left a live dispatch to be judged from a PR list.

The question: when the body argument names a file, what should `cairn comment` do?

## Decision Drivers

* **The report must not be lost quietly.** A dispatch that cannot be read is paid
  for twice or parked; the failure mode is money and stale work, not an ugly string.
* **One CLI, one convention.** Two positional arguments meaning two different
  things in the same tool is what produced the bug, and any documentation fix
  leaves the next caller to rediscover it.
* **Nothing a human typed should change meaning under their fingers.** Prose that
  mentions a path, a URL, or a filename should post as written.
* **Explicit stays explicit.** `--file` already exists and must keep working and
  keep meaning "this file is the body".

## Considered Options

1. **Leave the CLI alone and fix the callers** — tell lanes to pass text or use
   `--file`. Rejected: it fixes one instruction file, and every script, agent and
   person that guesses from the rest of the CLI hits it again. The evidence is two
   independent machines producing the same shape of mistake.
2. **Refuse a body that looks like a path** (exit 2). Rejected on its own: an
   unattended run that gets an error may end with no comment at all, which is worse
   for the manager than a comment whose body is a path.
3. **Read the argument when it names an existing file; otherwise post it as text,
   and say on stderr whenever the argument looked like a path.** Chosen.

## Decision

`cairn comment <id> <arg>` treats `<arg>` as a file when, and only when, all of
these hold:

* it is a single line, so a pasted multi-line report is never a path;
* trimmed, it begins with `/`, `./`, `../` or `~/`, so prose that mentions a bare
  filename or a URL is untouched;
* it resolves to an existing regular file (`~/` expands through the user's home).

Then its contents become the body and stderr notes which file was read. A file that
cannot be read is a usage error (exit 2) rather than a post of its own name.
Anything else — a missing path, a directory, ordinary prose — is posted verbatim,
and when it merely *looked* like a path the CLI warns on stderr instead of staying
silent, so the mistake stays visible without losing the report. `--file` is
unchanged and stays mutually exclusive with a body argument.

## Consequences

* The observed instances now post the report instead of the path, with no change to
  any caller.
* A comment whose entire body is a path to a file that exists on this machine is no
  longer reproducible as written; the CLI reads the file and says so. Nobody writes
  a comment that is only such a path, and `--file` plus the verbatim warning cover
  anyone who meant it.
* One more stderr note appears in agent transcripts, where today there is silence
  and a broken handoff.
* An argument naming a file that exists but cannot be read stays a usage error
  (exit 2) rather than the warn-and-post path. Posting the argument there would
  reproduce the observed bug verbatim: the manager would read a path as the whole
  report. A file that exists is unambiguously intended as the body, so failing
  loudly names the file and the errno where the lane's own report step surfaces
  it; the warn-and-post path is for the ambiguous case, prose that merely looks
  like a path or a file already gone.
* The read adds no file-access capability: `cairn <path>` and `cairn comment
  --file` already read any file the caller can read. This change only drops the
  need to type `--file`.
* Server behaviour, the API, and the storage of a comment body are untouched: this
  is argument handling, not annotation semantics.
