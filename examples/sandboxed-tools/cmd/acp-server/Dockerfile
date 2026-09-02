# Build from the repository root:
#   docker build -t <registry>/sandboxed-tools-acp-server:latest -f examples/sandboxed-tools/cmd/acp-server/Dockerfile .

# Build go binary. The builder runs on the build host's native platform and
# cross-compiles for the target, which is much faster than emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.0 AS builder
ARG TARGETARCH

WORKDIR /workspace

# Download dependencies first to take advantage of Docker layer caching.
# The root go.mod replaces the agentclientprotocol module with a local path,
# so its go.mod must be present for go mod download to resolve the graph.
COPY go.mod go.sum ./
COPY examples/agentclientprotocol/go.mod examples/agentclientprotocol/go.mod
RUN go mod download

# Copy the source packages the server depends on
COPY api/ api/
COPY clients/ clients/
COPY examples/agentclientprotocol/ examples/agentclientprotocol/
COPY examples/sandboxed-tools/ examples/sandboxed-tools/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -o /acp-server ./examples/sandboxed-tools/cmd/acp-server

# Use a minimal base image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /acp-server /acp-server

# Session state and filesystem snapshots are stored under $HOME; the
# deployment mounts a volume here.
ENV HOME=/data

ENTRYPOINT ["/acp-server"]
