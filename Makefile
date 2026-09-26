.PHONY: build test test-race test-js vet fmt fmt-check lint check tidy up down migrate ci verify-cli

GO ?= go
PKGS ?= ./...

# Optional path to a built cairn binary for `make verify-cli`. Without it only
# the import-graph gate runs, which needs no build.
CLI_BIN ?=

build:
	$(GO) build $(PKGS)

test:
	$(GO) test $(PKGS)

# Concurrency-sensitive code (streaming ingest) MUST pass under the race detector.
test-race:
	$(GO) test -race $(PKGS)

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
lint: fmt-check vet

# The source is private and only the CLI is distributed, so the shipped binary
# must never contain the server. This is cheap (an import-graph allowlist) and
# runs with the ordinary checks rather than only at release time, because the
# way it breaks is an ordinary-looking import added months earlier.
verify-cli:
	@scripts/verify-cli-artifact.sh $(CLI_BIN)

check: lint test verify-cli

tidy:
	$(GO) mod tidy

# Local dependencies: Postgres + MinIO (S3-compatible object storage).
up:
	docker compose up -d

down:
	docker compose down -v

ci: fmt-check vet build test-race test-js
