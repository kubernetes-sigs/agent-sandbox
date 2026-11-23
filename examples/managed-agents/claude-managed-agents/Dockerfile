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

# Build context: the repository root, because the example module replaces
# sigs.k8s.io/agent-sandbox with ../.. :
#   docker build -f examples/managed-agents/claude-managed-agents/Dockerfile -t cma-runner:v1 .

# The builder runs on the build host's native platform and cross-compiles
# for the target, which is much faster than emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
ARG TARGETARCH
WORKDIR /src
# The replaced root module's go.mod must exist for the example module to load.
COPY go.mod go.sum ./
COPY examples/managed-agents/claude-managed-agents/ ./examples/managed-agents/claude-managed-agents/
WORKDIR /src/examples/managed-agents/claude-managed-agents
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /runner ./cmd/runner

# The runtime needs a real shell and coreutils because CMA tool calls execute
# bash commands inside the sandbox. git is deliberately not installed: it
# roughly doubles the runtime layer, and agents that need it can install it
# themselves or use a derived image.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash coreutils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /runner /usr/local/bin/runner
RUN useradd -u 1000 -m -s /bin/bash sandbox \
    && mkdir -p /workspace \
    && chown -R 1000:1000 /workspace
USER 1000
WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/runner", "--addr", ":8080"]
