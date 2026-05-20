# Build the sandbox-router binary as a static distroless image. Build context
# MUST be the repo root because the binary imports api/, extensions/api/,
# clients/go/, and internal/ from the shared module — dev/tools/push-images
# is patched to pass `--context .` for this Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

# Declare TARGETARCH to make it available in this build stage.
ARG TARGETARCH

# Version info injected via build args (e.g. docker build --build-arg GIT_VERSION=...).
ARG GIT_VERSION=unknown
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

# Download dependencies first to leverage layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the code the binary needs. controllers/ and extensions/controllers/
# are NOT copied because sandbox-router does not depend on them.
COPY api/ ./api/
COPY extensions/api/ ./extensions/api/
COPY clients/go/ ./clients/go/
COPY internal/ ./internal/

# Build the binary with size and version-info optimizations.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
        -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} \
        -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} \
        -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /sandbox-router ./clients/go/sandbox-router

# Final image: distroless static so we ship only the binary plus minimal runtime.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /sandbox-router /sandbox-router

# Document the default ports the binary listens on. Operators can override
# via --http-bind-address / --https-bind-address / --metrics-bind-address /
# --health-probe-bind-address.
EXPOSE 8080 8443 9090 8081

ENTRYPOINT ["/sandbox-router"]
