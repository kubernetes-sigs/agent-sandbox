# Build from the repository root. For kind (kind.local/ is not a registry;
# the image is loaded into the cluster's nodes):
#   export IMAGE=kind.local/sandboxed-tools-acp-server:latest
#   docker build -t ${IMAGE} -f examples/sandboxed-tools/cmd/acp-server/Dockerfile .
#   kind load docker-image ${IMAGE} --name agent-sandbox
# For any other cluster, use a registry it can pull from and docker push
# instead of kind load.

# Build go binary. The builder runs on the build host's native platform and
# cross-compiles for the target, which is much faster than emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.5 AS builder
ARG TARGETARCH

WORKDIR /workspace

# Download dependencies first to take advantage of Docker layer caching.
# examples/sandboxed-tools is its own module that replaces the root module
# and the agentclientprotocol example with local paths, so all three go.mod
# files must be present for go mod download to resolve the graph.
COPY go.mod go.sum ./
COPY examples/agentclientprotocol/go.mod examples/agentclientprotocol/
COPY examples/sandboxed-tools/go.mod examples/sandboxed-tools/go.sum examples/sandboxed-tools/
RUN cd examples/sandboxed-tools && go mod download

# Copy the source packages the server depends on
COPY api/ api/
COPY clients/k8s/ clients/k8s/
COPY examples/agentclientprotocol/ examples/agentclientprotocol/
COPY examples/sandboxed-tools/ examples/sandboxed-tools/

WORKDIR /workspace/examples/sandboxed-tools
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -o /acp-server ./cmd/acp-server

# Use a minimal base image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /acp-server /acp-server

# Session state and filesystem snapshots are stored under $HOME; the
# deployment mounts a volume here.
ENV HOME=/data

ENTRYPOINT ["/acp-server"]
