# Build go binaries
FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

# Declare TARGETARCH to make it available in this build stage
ARG TARGETARCH

# Version info injected via build args
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

# Build both the controller manager and the sandbox agent daemon
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /agent-sandbox-controller ./cmd/agent-sandbox-controller

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /agent-sandbox-agent ./cmd/agent-sandbox-agent

# --- Target 1: The Controller Image ---
FROM gcr.io/distroless/static-debian13:nonroot AS controller
COPY --from=builder /agent-sandbox-controller /agent-sandbox-controller
ENTRYPOINT ["/agent-sandbox-controller"]

# --- Target 2: The Sandbox Agent Daemon Image ---
# We use debian-slim for the agent runtime container because it needs standard system tools 
# (like sh, python, jupyter) to execute user commands inside the sandbox filesystem.
FROM python:3.11-slim AS agent

# Install standard system testing tools and Jupyter
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    procps \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir jupyter-server ipykernel

# Copy the compiled Go daemon binary from builder stage
COPY --from=builder /agent-sandbox-agent /bin/agent-sandbox-agent

# Setup default workspace directory
RUN mkdir -p /workspace && chmod 777 /workspace
WORKDIR /workspace

EXPOSE 50051

ENTRYPOINT ["/bin/agent-sandbox-agent"]
CMD ["--port=50051"]
