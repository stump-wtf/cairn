# syntax=docker/dockerfile:1
#
# Single static Cairn binary (ADR-0012). Migrations and (future) web assets are
# embedded, so the runtime image is just the binary + CA certs.

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
# Cache module downloads before copying the full tree.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static, stripped, reproducible build.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd

FROM alpine:3.24
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S cairn && adduser -S -G cairn cairn
COPY --from=build /out/cairnd /usr/local/bin/cairnd
USER cairn
ENV CAIRN_HTTP_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/usr/local/bin/cairnd"]
