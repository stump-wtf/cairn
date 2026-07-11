# Cairn server image. Multi-stage: static Go build → minimal Alpine runtime
# (ca-certificates for S3-over-HTTPS + wget for the container healthcheck).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cairnd ./cmd/cairnd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -u 3000 cairn
COPY --from=build /out/cairnd /usr/local/bin/cairnd
USER cairn
EXPOSE 8080
# cairnd reads CAIRN_* env (DB, S3, base URL); serves /healthz.
ENTRYPOINT ["cairnd"]
