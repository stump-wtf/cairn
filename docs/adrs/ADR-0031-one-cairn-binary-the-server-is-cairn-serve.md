---
status: accepted
date: 2026-09-29
decision-makers: [joestump]
supersedes: [ADR-0020 (in part — the two-binary split)]
amends: [ADR-0003, ADR-0012]
related: [SPEC-0008, SPEC-0015]
---

# ADR-0031: One Cairn Binary — The Server Is `cairn serve`

## Context and Problem Statement

Cairn builds two Go binaries: `cairn` (`./cmd/cairn`, the CLI, a pure REST
client) and `cairnd` (`./cmd/cairnd`, the server). Releases publish both, to a
separate public repo (`stump-wtf/cairn-cli`) that exists only because the
binaries do, and the Homebrew tap renders its formula from that repo's assets.
Every other service in the fleet — `switchboard serve` plus operator
subcommands, `harness daemon` plus its CLI — is one binary.

Joe, 09/29: "Cairn shouldn't have a separate CLI. Go is nice because it's a
single binary paradigm."

The costs of the split are real and recurring: a second release destination
whose only content is cairn's own binaries; a tap formula that must be told
which of two repos to render from; a `verify-cli` gate (scripts/
verify-cli-artifact.sh) whose whole job is keeping the server out of the CLI
binary — a dependency invariant that becomes false the moment the split ends;
and an image entrypoint (`cairnd`) that is one more name operators must know.
The benefit was decoupling the CLI's download size and release cadence from the
server. With the source MIT and the whole product one repository, that benefit
no longer buys enough to pay for the machinery.

## Decision

**One binary: `cairn`.** The server becomes a subcommand, `cairn serve`,
matching `switchboard serve`. Every existing CLI command stays exactly where it
is — `cairn add`, `cairn login`, and the root pipe-and-file ingest are
untouched.

* **Command surface.** `cairn serve` runs the server. `cairn serve --help`
  documents it. Operator subcommands that the specs design for `cairnd`
  (search reindex, retention admin) are `cairn <subcommand>` under the same
  tree; the specs are amended to say so.
* **Config and env compatibility (the hard requirement).** Every `CAIRN_*`
  environment variable and every flag `cairnd` accepts works under
  `cairn serve` unchanged. Configuration continues to come from the
  environment only; `cairn serve` takes no flags of its own beyond the
  standard `--help`/`--version`, so a deployment's env is the contract and
  does not move.
* **Exit behaviour.** `cairn serve` keeps cairnd's exit contract: startup
  failures exit non-zero with the same error text, SIGTERM/SIGINT shut down
  gracefully, and the process logs the same structured JSON lines.
* **Image.** The container image ships the single `cairn` binary. The image
  **also ships a `cairnd` shim** — a tiny executable that `exec`s
  `cairn serve "$@"` — and **keeps `cairnd` as the default ENTRYPOINT**, so
  every existing deployment (the StumpCloud edge stack runs the image with no
  command override; self-hosters' compose files name the service, not the
  command) continues to start the server without a change. Removing the shim
  is a later, separately-announced breaking change.
* **Release destination.** goreleaser publishes **one** build (`cairn`) from
  `stump-wtf/cairn` itself — the same repo the source lives in — and the tap
  formula renders from those assets. The `cairnd_<ver>_<os>_<arch>` archives
  stop being published. `stump-wtf/cairn-cli` is retired afterwards (by Joe,
  not the release pipeline).
* **The import-graph CLI gate is retired with the split.**
  scripts/verify-cli-artifact.sh enforced "the shipped CLI contains no server
  code". That invariant is not merely relaxed — it is *reversed*: the shipped
  binary is supposed to contain the server. The gate is removed in the same
  change that folds the binaries in, and the archive-composition check
  (scripts/verify-release-archives.sh) is narrowed to the one remaining
  archive shape.

## Consequences

* **Easier to get.** One download, one tap formula source, one name. The
  release notes stop needing a "which download do I want" header with two
  answers.
* **Larger CLI download.** The binary now carries the server (embedded docs,
  migrations, the HTTP stack) — a few MB. This is the accepted trade; the CLI
  is no longer pretending to be separable from the product.
* **The `cairnd` name deprecates, in stages:** (1) this release, the image
  keeps the `cairnd` shim and entrypoint so deploys are untouched; (2) after
  the v0.3.0 image has shipped everywhere, StumpCloud's Ansible flips to an
  explicit `command: ["serve"]`-style invocation and the shim is removed;
  (3) `stump-wtf/cairn-cli` is archived. Native `cairnd` archive users move to
  `cairn serve` with no config change — the env contract is identical.
* **One ADR-0020 promise is deliberately broken.** ADR-0020 chose to embed the
  docs in the server binary and explicitly kept the CLI separate, on the
  release-cadence argument. This ADR supersedes that clause; the docs
  embedding itself stands, now served by `cairn serve`.
* **ADR-0003's "pure network client" clause narrows.** The CLI's *commands*
  remain pure REST clients with no domain logic (SPEC-0008 stands); what
  changes is that the binary also carries the server behind `serve`. Surface
  parity is unaffected — the parity claim was always about semantics, not
  linkage.
* **Specs move with the decision.** Every spec that names `cairnd` as the
  process or as a command prefix is amended in the same change that lands
  this: the server process is "the cairn server (`cairn serve`)" and the
  operator surface is `cairn <subcommand>`.
