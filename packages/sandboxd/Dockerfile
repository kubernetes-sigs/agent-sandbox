# Build the sandboxd portable runtime daemon (KEP-539.2). Build context MUST
# be the repo root because the binary imports internal/ and the generated
# specs from the shared Go module — dev/tools/push-images is patched to pass
# `--context .` for this Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.26.5 AS builder

# Declare TARGETARCH to make it available in this build stage.
ARG TARGETARCH

# Version info injected via build args (e.g. docker build --build-arg GIT_VERSION=...).
ARG GIT_VERSION=unknown
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Download dependencies first to leverage layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the code the binary needs: the daemon itself plus internal/ for
# version stamping.
COPY packages/sandboxd/ ./packages/sandboxd/
COPY internal/ ./internal/

# Build the binary with size and version-info optimizations.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
        -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} \
        -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} \
        -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /sandboxd ./packages/sandboxd/cmd/sandboxd

# Runtime image. NOT distroless: the ProcessService executes user commands,
# which need a shell and coreutils; procps supplies ps/kill for process
# management and debugging.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /sandboxd /usr/local/bin/sandboxd

# The sandbox workspace root (--root-dir default). Owned by the non-root
# runtime user so agents can write freely.
RUN mkdir -p /workspace && chown 1000:1000 /workspace
WORKDIR /workspace
USER 1000

# Document the default localhost-only ports: REST (:8080) and gRPC (:9090).
# Overridable via --rest-addr / --grpc-addr.
EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/sandboxd"]
