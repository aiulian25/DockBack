# syntax=docker/dockerfile:1
#
# DockBack — multi-stage build producing a single static binary in a
# distroless image (PLAN §1.4): no shell, no package manager, runs non-root.
#
# Base images are PINNED BY DIGEST (tag kept for readability) for reproducible,
# supply-chain-safe builds — a repushed tag can't silently change the build
# (Security.md §2/§5, PLAN §10.2).
# Refresh cadence: review monthly, or whenever `make scan` flags a base-image CVE.
# Get a fresh digest with:
#   docker pull <img>:<tag> && docker inspect --format '{{index .RepoDigests 0}}' <img>:<tag>

# ---- Stage 1: build the React frontend -------------------------------------
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS frontend
WORKDIR /app/frontend
# The lockfile is required, not optional: a missing one fails here with a clear
# message rather than inside npm.
COPY frontend/package.json frontend/package-lock.json ./
# npm ci and nothing else. The old `|| npm install` fallback turned a stale
# lockfile into a silent, unpinned, non-reproducible dependency set; failing is
# the correct signal.
RUN npm ci
COPY frontend/ ./
# Vite emits straight into the Go embed dir.
RUN npm run build

# ---- Stage 2: build the Go binary (CGO off, embeds the React build) --------
FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS backend
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Bring in the freshly built UI so //go:embed picks it up.
COPY --from=frontend /app/internal/web/dist ./internal/web/dist
# App version: pass --build-arg VERSION=v1.2.3 (e.g. from a git tag) when shipping;
# defaults to "dev" so unstamped builds never show a fake number.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOFLAGS=-mod=mod go build \
    -ldflags="-s -w -X dockback/internal/version.Version=${VERSION}" \
    -trimpath \
    -o /out/dockback .
# Seed the writable dirs with the non-root owner. Docker copies this ownership
# onto a fresh named volume on first mount, so no init/chown sidecar is needed.
RUN mkdir -p /seed/data /seed/backups && chown -R 65532:65532 /seed

# ---- Stage 3: distroless runtime, non-root ---------------------------------
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
# nonroot uid:gid is 65532:65532 (PLAN §2.3).
COPY --from=backend /out/dockback /dockback
# Pre-created, correctly-owned volume mountpoints (see seed step above).
COPY --from=backend --chown=65532:65532 /seed/data /app/data
COPY --from=backend --chown=65532:65532 /seed/backups /app/backups

# Writable paths are provided as volumes/tmpfs by compose (read-only rootfs).
ENV DOCKBACK_DATA_DIR=/app/data \
    DOCKBACK_BACKUPS_DIR=/app/backups \
    DOCKBACK_TMP_DIR=/tmp \
    HOST=0.0.0.0 \
    PORT=28734

EXPOSE 28734
USER 65532:65532
# Distroless has no shell/curl, so the binary probes its own /healthz (exec form).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/dockback", "-healthcheck"]
ENTRYPOINT ["/dockback"]
