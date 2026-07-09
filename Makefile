.PHONY: build test test-race vet fmt fmt-check lint tidy up down migrate ci

GO ?= go
PKGS ?= ./...

build:
	$(GO) build $(PKGS)

test:
	$(GO) test $(PKGS)

# Concurrency-sensitive code (streaming ingest) MUST pass under the race detector.
test-race:
	$(GO) test -race $(PKGS)

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

ci: fmt-check vet build test-race
