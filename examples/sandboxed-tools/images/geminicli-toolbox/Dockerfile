# Sandbox image for the "geminicli" toolset of the sandboxed-tools example.
#
# It bundles the geminicli-toolbox binary (the Go implementation of the
# gemini-cli filesystem tools, see
# examples/sandboxed-tools/pkg/toolsets/geminicli/toolbox) on top of a small
# Debian base that provides bash and git for run_shell_command.
#
# Build from the REPOSITORY ROOT (the build needs the Go module context):
#
#   docker build -t geminicli-toolbox:latest \
#     -f examples/sandboxed-tools/images/geminicli-toolbox/Dockerfile .
#
# For a kind cluster (e.g. created by `make deploy-kind`):
#
#   kind load docker-image geminicli-toolbox:latest --name agent-sandbox
#
# Then run the example with the geminicli toolset, which uses this image by
# default:
#
#   go run ./examples/sandboxed-tools/cmd/sandboxed-tools-cli \
#     -session mysession -toolset geminicli

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY examples/sandboxed-tools ./examples/sandboxed-tools
# -buildvcs=false: the build context includes .git, but git metadata is not
# usable inside the build container (and version stamping is not needed here).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" \
      -o /geminicli-toolbox ./examples/sandboxed-tools/cmd/geminicli-toolbox

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git curl procps \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /geminicli-toolbox /usr/local/bin/geminicli-toolbox

# Note: we run as root; we can later investigate running as non-root
# and whether this reduces functionality / eval-scores in the sandbox.

# The sandboxed-tools agent overrides the command with "sleep infinity" and
# execs tools into the container; this default just keeps the container alive
# if the image is run directly.
CMD ["sleep", "infinity"]
