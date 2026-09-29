.PHONY: build test test-race test-js test-scripts vet fmt fmt-check lint tidy-check check tidy up down migrate ci verify-cli release-snapshot website

GO ?= go
PKGS ?= ./...

# Extra flags for `go test`, empty by default so `make test` is plain
# `go test ./...` from a clean checkout. CI's integration job passes an explicit
# -timeout here: go's default is 10m per test binary, and a shared runner under
# load can push internal/httpapi past that without anything being hung.
GOTESTFLAGS ?=

# Optional path to a built cairn binary for `make verify-cli`. Without it only
# the import-graph gate runs, which needs no build.
CLI_BIN ?=

build:
	$(GO) build $(PKGS)

test:
	$(GO) test $(GOTESTFLAGS) $(PKGS)

# Concurrency-sensitive code (streaming ingest) MUST pass under the race detector.
test-race:
	$(GO) test -race $(GOTESTFLAGS) $(PKGS)

# The trajectory viewer's pure scrubber math has a plain-node unit test (no
# DOM). Skipped silently when node is absent, so a Go-only box still passes.
#
# It lives in web/jstests/, NOT beside the code in web/assets/: that directory
# is embedded wholesale (`//go:embed web/assets` in web.go) and served at
# /assets/*, so a *_test.js sitting there ships inside the production binary and
# is publicly fetchable. TestAssetsShipNoTestFiles guards that boundary.
test-js:
	@if command -v node >/dev/null 2>&1; then \
		for f in internal/httpapi/web/jstests/*_test.js; do \
			[ -e "$$f" ] || continue; node "$$f" || exit 1; \
		done; \
	else \
		echo "node not found; skipping JS unit tests"; \
	fi

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

# CI gate: fail if anything is not gofmt-clean.
fmt-check:
	@out="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

# Uniform entry points: every repo answers to `make lint` / `make check`.
# tidy-check is here because goreleaser runs `go mod tidy` as a release hook
# (.goreleaser.yaml before hooks): an untidy module dirties the tree mid-release
# and goreleaser aborts with "git is in a dirty state", so the tag ships
# nothing. Caught on every PR instead.
lint: tidy-check fmt-check vet

# The source is private and only the CLI is distributed, so the shipped binary
# must never contain the server. This is cheap (an import-graph allowlist) and
# runs with the ordinary checks rather than only at release time, because the
# way it breaks is an ordinary-looking import added months earlier.
verify-cli:
	@scripts/verify-cli-artifact.sh $(CLI_BIN)

# Tests for the release scripts themselves (scripts/*.test.sh): fixtures each
# gate must pass or fail, each for its own reason. No Go build, so it is cheap
# enough for every run. A glob, so a new script's tests join by existing.
test-scripts:
	@set -e; n=0; for t in scripts/*.test.sh; do \
		[ -e "$$t" ] || continue; echo "==> $$t"; "$$t"; n=$$((n + 1)); \
	done; [ "$$n" -gt 0 ] || { echo "no scripts/*.test.sh found; nothing was tested"; exit 1; }

# A local dry run of the whole release: both builds, both archives, the CLI
# post hook, then the archive-composition check the release job runs before it
# publishes. Needs goreleaser on PATH; publishes nothing.
release-snapshot:
	goreleaser release --snapshot --clean
	scripts/verify-release-archives.sh dist

check: lint test verify-cli test-scripts

tidy:
	$(GO) mod tidy

# Fail when `go mod tidy` would change go.mod or go.sum, restoring whatever
# it touched so the working tree is left as the caller had it.
tidy-check:
	@d=$$(mktemp -d); cp go.mod go.sum $$d/; $(GO) mod tidy; \
	s=0; cmp -s go.mod $$d/go.mod || s=1; cmp -s go.sum $$d/go.sum || s=1; \
	cp $$d/go.mod $$d/go.sum .; rm -rf $$d; \
	if [ $$s -ne 0 ]; then echo "go mod tidy would change go.mod/go.sum: run 'make tidy' and commit the result"; exit 1; fi

# Local dependencies: Postgres + MinIO (S3-compatible object storage).
up:
	docker compose up -d

down:
	docker compose down -v

ci: fmt-check vet build test-race test-js test-scripts

# The docs site's own gate: its unit tests, the typecheck, and the full build.
# The build is what runs the design-record validator (a front-matter edge to a
# record that does not exist throws) and, from its postBuild hook, the bundle
# scan for private hosts and credentials. Not part of `ci` or `check`, so a
# Go-only box still passes those; the pipeline runs it as its own `website` job.
# DOCS_URL / DOCS_BASE_URL match the public build, not the Pages one.
website:
	cd website && npm ci --no-audit --no-fund && npm test && npm run typecheck && \
		DOCS_URL=https://cairn.stump.wtf DOCS_BASE_URL=/ npm run build
