# Build stage. Debian-based so the race detector (which requires CGO + glibc)
# can be enabled via --build-arg RACE=-race.
FROM golang:1.26-bookworm AS builder

# Build arguments for version injection
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG GIT_TREE_STATE=unknown
ARG BUILD_DATE=unknown
# RACE: pass --build-arg RACE=-race to produce a race-instrumented binary.
# Empty (default) builds the normal static binary.
ARG RACE=""

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
# default build keeps CGO_ENABLED=0 and is statically linked. GOARCH is left
# unset so Go targets the buildx target architecture — race builds need a
# cgo toolchain that matches, and the kind cluster on Apple Silicon runs
# arm64 not amd64.
RUN CGO_ENABLED=$([ -n "$RACE" ] && echo 1 || echo 0) GOOS=linux \
    go build ${RACE} \
    -ldflags="-X 'go.miloapis.com/ipam/internal/version.Version=${VERSION}' \
              -X 'go.miloapis.com/ipam/internal/version.GitCommit=${GIT_COMMIT}' \
              -X 'go.miloapis.com/ipam/internal/version.GitTreeState=${GIT_TREE_STATE}' \
              -X 'go.miloapis.com/ipam/internal/version.BuildDate=${BUILD_DATE}'" \
    -a -o ipam ./cmd/ipam

# Runtime stage. distroless/base ships glibc so it works for both the default
# CGO_ENABLED=0 static build and the CGO_ENABLED=1 race build.
FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /
COPY --from=builder /workspace/ipam .
USER 65532:65532

ENTRYPOINT ["/ipam"]
