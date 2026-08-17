# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Multi-stage build: compile envd from upstream E2B source, then package
# into a minimal runtime image with GNU userland (needed for process exec).
#
# Build:
#   docker build -t envd-sandbox:latest .
#   docker build -t envd-sandbox:latest --build-arg ENVD_VERSION=main .

# Stage 1: Build envd binary from e2b-dev/infra source.
# NOTE: For production use, pin ENVD_VERSION to a specific commit or release
# tag (e.g., --build-arg ENVD_VERSION=v0.1.0 or a commit hash) instead of
# tracking the moving `main` branch.
FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

# Pinned to a known-good commit. Override with --build-arg ENVD_VERSION=<ref>
# to track upstream or pin to a different revision.
ARG ENVD_VERSION=411b63edf6dea8edd265d1c5e442f41a80b76b2a
ARG TARGETARCH=amd64

WORKDIR /src

# envd lives under packages/envd in the e2b-dev/infra monorepo.
# Clone then checkout supports branches, tags, and commit hashes.
RUN git clone https://github.com/e2b-dev/infra.git /src/infra \
    && cd /src/infra \
    && git checkout ${ENVD_VERSION}

WORKDIR /src/infra/packages/envd

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOWORK=off GOTOOLCHAIN=auto \
      go build -ldflags="-s -w" -trimpath -o /out/envd .

# Stage 2: Runtime image with GNU userland for process exec support.
# NOTE: debian:12-slim == debian:bookworm-slim (same image, different tag).
FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      bash \
      coreutils \
      findutils \
      grep \
      sed \
      tar \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/envd /usr/local/bin/envd

# Create a non-root user for subprocess execution.
# Following the e2b-dev debug.Dockerfile pattern:
#   https://github.com/e2b-dev/infra/blob/main/packages/envd/debug.Dockerfile
#
# Security model: envd runs as root (required for PTY, process management),
# but user code spawned via /init's defaultUser runs as this unprivileged user.
# This limits blast radius if user code is malicious.
RUN useradd -ms /bin/bash user
RUN chmod 777 -R /home/user

WORKDIR /home/user

# 49983 is envd's default HTTP/gRPC port.
EXPOSE 49983

# --isnotfc     : skip Firecracker MMDS polling (we're running in a plain
#                 container, not a Firecracker microVM).
# --no-cgroups  : skip cgroup v2 setup (not available in sandbox containers).
# --verbose     : useful for debugging; remove in production.
ENTRYPOINT ["/usr/local/bin/envd"]
CMD ["--isnotfc", "--no-cgroups", "--port", "49983", "--verbose"]
