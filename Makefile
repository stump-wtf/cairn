.PHONY: build test test-race test-js vet fmt fmt-check lint tidy up down migrate ci

GO ?= go
PKGS ?= ./...

build:
	$(GO) build $(PKGS)

test:
	$(GO) test $(PKGS)

# Concurrency-sensitive code (streaming ingest) MUST pass under the race detector.
test-race:
	$(GO) test -race $(PKGS)

# The trajectory viewer's pure scrubber math has a plain-node unit test (no
# DOM). Skipped silently when node is absent, so a Go-only box still passes.
test-js:
	@if command -v node >/dev/null 2>&1; then \
		node internal/httpapi/web/assets/trajectory_js_test.js; \
	else \
		echo "node not found; skipping trajectory_js_test.js"; \
	fi

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

# CI gate: fail if anything is not gofmt-clean.
fmt-check:
	@out="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

tidy:
	$(GO) mod tidy

# Local dependencies: Postgres + MinIO (S3-compatible object storage).
up:
	docker compose up -d

down:
	docker compose down -v

ci: fmt-check vet build test-race test-js
