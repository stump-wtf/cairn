# syntax=docker/dockerfile:1
#
# One static Cairn binary (ADR-0012, ADR-0031): the server is `cairn serve`.
# Migrations and (future) web assets are embedded, so the runtime image is
# just the binary + CA certs.
#
# DEPLOY SAFETY (ADR-0031): the image keeps /usr/local/bin/cairnd as its
# ENTRYPOINT, built from cmd/cairnd — a shim that execs `cairn serve "$@"`.
# The StumpCloud edge stack (stumpcloud/ansible, compose.yaml.j2) runs this
# image with no command override, so the entrypoint is the deploy contract;
# existing deployments start the server unchanged.

FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
# Cache module downloads before copying the full tree.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static, stripped, reproducible builds: the one binary, plus the deprecated
# cairnd shim that execs it (deploy safety above).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cairn ./cmd/cairn \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd

FROM alpine:3.24
# `apk upgrade` first: a base image tag is only as fresh as its last rebuild,
# and the main-only Trivy gate (--ignore-unfixed, HIGH/CRITICAL, exit 1)
# fails on any CVE the repository has already fixed. alpine 3.24.1 as pulled
# shipped libssl3/libcrypto3 3.5.7-r0 (CVE-2026-14456, fixed in 3.5.8-r0).
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates wget \
    && addgroup -S cairn && adduser -S -G cairn cairn
COPY --from=build /out/cairn /usr/local/bin/cairn
COPY --from=build /out/cairnd /usr/local/bin/cairnd
USER cairn
ENV CAIRN_HTTP_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/cairnd"]
