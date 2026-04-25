# Build go binaries
# See https://github.com/golang/go/issues/69255#issuecomment-2523276831
FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

# Declare TARGETARCH to make it available in this build stage
ARG TARGETARCH

# Version info injected via build args (e.g. docker build --buildarg GIT_VERSION=...)
ARG GIT_VERSION=unknown
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

# Download dependencies first to leverage layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the code source needed
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY controllers/ ./controllers/
COPY extensions/ ./extensions/
COPY internal/ ./internal/

# Build the binary with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /agent-sandbox-controller ./cmd/agent-sandbox-controller


# The controller image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /agent-sandbox-controller /agent-sandbox-controller

ENTRYPOINT ["/agent-sandbox-controller"]
