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
# sigs.k8s.io/agent-sandbox with ../../.. :
#   docker build -f examples/managed-agents/bedrock-managed-agents/Dockerfile -t bma-runner:v1 .

# The builder runs on the build host's native platform and cross-compiles
# for the target, which is much faster than emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETARCH
WORKDIR /src
# The replaced root module's go.mod must exist for the example module to load.
COPY go.mod go.sum ./
COPY examples/managed-agents/bedrock-managed-agents/ ./examples/managed-agents/bedrock-managed-agents/
WORKDIR /src/examples/managed-agents/bedrock-managed-agents
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /runner ./cmd/runner
# fakeexec is the credential-free stand-in for `codex exec-server` used by
# the link-level e2e (RUNNER_EXEC_BIN selects between them at runtime).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /fakeexec ./internal/fakeexec

# The runtime needs Node.js because the codex executor ships as an npm
# package, plus a real shell and coreutils because the agent's tool calls
# run bash inside the sandbox. The codex version is pinned: BMA requires
# >= 0.154.0, the first line with the exec-server AWS SigV4 flags.
FROM node:22-bookworm-slim
ARG CODEX_VERSION=0.172.0
# The node base already ships bash and coreutils; only the CA bundle is
# missing, and the executor needs it for TLS to the platform.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The cache mount keeps the download out of the layer, so rebuilds after a
# version bump reuse whatever is already local.
RUN --mount=type=cache,target=/root/.npm \
    npm install -g "@openai/codex@${CODEX_VERSION}"
COPY --from=build /runner /usr/local/bin/runner
COPY --from=build /fakeexec /usr/local/bin/fakeexec
# The node base image already ships a UID 1000 user ("node"); reuse it so
# the pod's fsGroup/securityContext expectations match the other bridges'.
RUN mkdir -p /workspace \
    && chown -R 1000:1000 /workspace
# RUNNER_EXEC_BIN selects the executor binary; the e2e overrides it to
# /usr/local/bin/fakeexec so the link-level test needs no AWS credentials.
ENV RUNNER_EXEC_BIN=codex
USER 1000
WORKDIR /workspace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/runner", "--addr", ":8080"]
