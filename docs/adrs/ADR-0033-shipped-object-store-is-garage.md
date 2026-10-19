---
status: proposed
date: 2026-10-19
decision-makers: [joestump]
extends: [ADR-0008]
related: [ADR-0012, ADR-0020]
---

# ADR-0033: The Object Store Cairn Ships Is Garage

## Context and Problem Statement

Both compose paths failed at `docker compose pull` (issue #388):
`quay.io/minio/minio:latest`, which the self-hosting guide names, answers 401 to
anonymous pulls, and `minio/minio:latest` plus `minio/mc:latest`, which the
development stack names, report that the repository does not exist. ADR-0008
chose "any S3-compatible store" for the *interface* and never named the store the
repository ships, so the guide and `docker-compose.yml` each carried an unstated
assumption about one vendor's registry, and both rotted at once. The documented
deploy path and CI's integration job had also diverged: CI has run Garage
(`dxflrs/garage:v1.0.1`) since the integration job existed, which is why "S3
compatible" was a tested claim only in CI while a fresh machine could not follow
the guide.

## Decision Drivers

* A self-hoster copying the guide reaches a working instance on the first try,
  with images that pull anonymously from a registry nobody in this project
  controls.
* The store the documentation names is the store cairn's S3 code is actually
  exercised against, rather than a second vendor assumed compatible.
* Self-hosting stays possible on an existing store: ADR-0008's "Garage / MinIO /
  S3" surface is unchanged, and cairn's object-store code still speaks plain S3.
* Bootstrap steps belong in a script or a documented command list, not in an
  image that has to be pulled from somewhere.

## Considered Options

* **Garage, pinned to the tag CI runs.** The image resolves today, and it is the
  store the S3 tests have been running against all along, so the switch also
  removes a second vendor whose compatibility was only assumed. Cost: Garage needs a layout, a
  bucket and an imported key before it serves anything, and its image is
  distroless, so the bootstrap runs the admin CLI through `docker compose exec`
  rather than as a one-shot sidecar.
* **Pin a MinIO image by digest from a registry that still serves one.** Keeps
  today's zero-bootstrap experience, but no registry checked resolves
  anonymously, and pinning a frozen MinIO buys a maintenance debt against a
  project whose upstream images this issue just showed to be unreliable.
* **A different self-bootstrapping S3 image (LocalStack, Scality S3, Ceph RGW).**
  Each pulls, and each is either heavier than cairn's whole stack or a mock that
  does not implement the lifecycle call the staging TTL (ADR-0008) relies on.
  None is what CI tests.

## Decision

Name `dxflrs/garage:v1.0.1`, the tag the integration job already runs, in both
`docker-compose.yml` and the self-hosting guide's compose block. A test asserts
the tag matches across those two files and `.gitea/workflows/pipeline.yaml`, so
the documented path and the tested path cannot drift again without failing a
build. `scripts/s3-bootstrap.sh` owns the layout, bucket and key steps, and is
idempotent because `make up` runs it on every start.

Two things follow and are open for review:

* the development stack generates a key pair into a gitignored
  `garage/dev-credentials.env` instead of carrying a literal pair the way
  `minioadmin` was carried before, since a Garage secret is a 64-character hex
  string and a committed one is exactly what the secret scanner exists to catch;
* the built-in `minioadmin` defaults in `internal/config` stay, because a
  self-hoster pointing cairn at their own MinIO still needs no configuration for
  credentials, and the guide now says plainly that those defaults cannot
  authenticate against the store it ships.

## Consequences

* Self-hosting gains one documented bootstrap step that MinIO did not need. It
  takes seconds, it is idempotent, and it is the store's own cluster layout
  rather than anything cairn participates in.
* Anyone who prefers MinIO, Garage on other hosts, or S3 itself substitutes the
  service block and drops the bootstrap step; nothing in cairn changed to favor
  Garage beyond which image the examples name.
* If a future issue shows Garage's registry as unreliable as MinIO's proved to
  be, the same reasoning applies to whatever resolves and is tested.
