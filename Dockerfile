# syntax=docker/dockerfile:1

# Build arguments for version control
# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.27
ARG HAPROXY_IMAGE=haproxytech/haproxy-debian:must-be-supplied@sha256:0000000000000000000000000000000000000000000000000000000000000000
ARG GIT_COMMIT=unknown
ARG GIT_TAG=unknown
ARG SOURCE_HASH=unknown

# -----------------------------------------------------------------------------
# Builder stage - compile the Go binary
# -----------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder

# go.mod can advance before the matching Docker patch image is published.
ENV GOTOOLCHAIN=auto

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Leverage Docker cache for Go modules
# Copy go.mod and go.sum first to cache module downloads
# Note: We intentionally avoid --mount=type=cache here so that downloaded
# modules become part of the layer and can be cached in CI registry caching.
COPY go.mod go.sum ./
# go mod download may populate go.sum even when the later build is read-only.
RUN cp go.mod /tmp/go.mod.before && \
    cp go.sum /tmp/go.sum.before && \
    go mod download && \
    cmp go.mod /tmp/go.mod.before && \
    cmp go.sum /tmp/go.sum.before

# Copy only source directories needed for compilation
# (explicit copies avoid cache invalidation from README, docs, tests, etc.)
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/

# Build arguments for cross-compilation and version info
ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM
ARG GIT_COMMIT
ARG GIT_TAG
ARG SOURCE_HASH

# Build the controller binary
# - CGO_ENABLED=0: static binary, no C dependencies
# - GOOS/GOARCH: cross-compilation for target platform
# - -trimpath: remove file system paths from binary
# - -buildvcs=false: reproducible builds (no VCS info embedded)
# - -pgo=auto: enable profile-guided optimization if default.pgo exists
# - -ldflags: linker flags for optimization and version info
#   - -s: strip debug information
#   - -w: strip DWARF debug information
#   - -X: inject version variables
# Output to platform-structured path for compatibility with goreleaser dockers_v2
RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build \
    -mod=readonly \
    -trimpath \
    -buildvcs=false \
    -pgo=auto \
    -ldflags="-s -w -X main.version=${GIT_TAG} -X main.commit=${GIT_COMMIT} -X main.sourceHash=${SOURCE_HASH}" \
    -o /build/${TARGETPLATFORM}/haptic \
    ./cmd/haptic

# -----------------------------------------------------------------------------
# Binary output stage - exports the controller binary
# This stage can be overridden via --build-context binary=<path> to use a
# pre-compiled binary instead of building from source (used in GitLab CI).
# The platform-structured path (e.g., linux/amd64/haptic) ensures
# compatibility with both CI builds and goreleaser dockers_v2.
# -----------------------------------------------------------------------------
FROM scratch AS binary
ARG TARGETPLATFORM
COPY --from=builder /build/${TARGETPLATFORM}/haptic /${TARGETPLATFORM}/haptic

# -----------------------------------------------------------------------------
# Runtime stage - minimal image with HAProxy for validation
# -----------------------------------------------------------------------------
FROM ${HAPROXY_IMAGE} AS runtime

# TARGETPLATFORM is set automatically by buildx (e.g., linux/amd64)
ARG TARGETPLATFORM

# Copy the controller binary from the 'binary' stage
# When using --build-context binary=<path>, this copies from the external context
COPY --chmod=0755 --from=binary /${TARGETPLATFORM}/haptic /usr/local/bin/haptic

# Shipped 0.1 Deployments retain this command during controller image preparation.
RUN ln -s haptic /usr/local/bin/haptic-controller

# Bundle the Helm chart so `haptic migrate-check` can render the
# config in-process with the image's own chart — no cluster or mounted
# values needed for the zero-argument audit. The path matches
# embeddedChartPath in cmd/haptic/chartrender.go. The chart is source
# YAML (~3 MB); it adds one layer and no runtime cost for `run`.
COPY charts/haptic /usr/share/haptic/chart

# Switch to haproxy user for security
# The haproxy user is pre-created by the haproxytech base image
USER haproxy

# WORKDIR creates the validation directories with ownership from USER.
WORKDIR /usr/local/etc/haproxy/maps
WORKDIR /usr/local/etc/haproxy/certs
WORKDIR /usr/local/etc/haproxy/general
WORKDIR /

# Override STOPSIGNAL from base image (SIGUSR1) to SIGTERM
# The haproxy base image uses SIGUSR1 for HAProxy's graceful shutdown,
# but our Go controller expects SIGTERM for graceful shutdown
STOPSIGNAL SIGTERM

# Set the entrypoint to the controller
ENTRYPOINT ["/usr/local/bin/haptic"]

# Default command (can be overridden)
CMD ["run"]
