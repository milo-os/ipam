# Build stage. Debian-based so the race detector (which requires CGO + glibc)
# can be enabled via --build-arg RACE=-race.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

# Build arguments for version injection
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown
# RACE: pass --build-arg RACE=-race to produce a race-instrumented binary.
# Empty (default) builds the normal static binary.
ARG RACE=""
# Cross-compilation targets — set automatically by docker buildx for
# multi-platform builds, enabling native Go cross-compilation without QEMU.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Cache dependencies
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY internal/ internal/
COPY migrations/ migrations/

# Build the binary. Race builds need CGO (and a real libc at runtime); the
# default build keeps CGO_ENABLED=0 and is statically linked.
# For non-race builds, GOARCH=$TARGETARCH enables native Go cross-compilation
# on the amd64 build host — no QEMU needed for arm64.
RUN if [ -n "$RACE" ]; then \
      CGO_ENABLED=1 GOOS=linux \
      go build ${RACE} \
        -ldflags="-X 'go.miloapis.com/ipam/internal/version.Version=${VERSION}' \
                  -X 'go.miloapis.com/ipam/internal/version.GitCommit=${GIT_COMMIT}' \
                  -X 'go.miloapis.com/ipam/internal/version.GitTreeState=${GIT_TREE_STATE}' \
                  -X 'go.miloapis.com/ipam/internal/version.BuildDate=${BUILD_DATE}'" \
        -o ipam ./cmd/ipam ; \
    else \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build \
        -ldflags="-X 'go.miloapis.com/ipam/internal/version.Version=${VERSION}' \
                  -X 'go.miloapis.com/ipam/internal/version.GitCommit=${GIT_COMMIT}' \
                  -X 'go.miloapis.com/ipam/internal/version.GitTreeState=${GIT_TREE_STATE}' \
                  -X 'go.miloapis.com/ipam/internal/version.BuildDate=${BUILD_DATE}'" \
        -o ipam ./cmd/ipam ; \
    fi

# Runtime stage. distroless/base ships glibc so it works for both the default
# CGO_ENABLED=0 static build and the CGO_ENABLED=1 race build.
FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /
COPY --from=builder /workspace/ipam .
USER 65532:65532

ENTRYPOINT ["/ipam"]
