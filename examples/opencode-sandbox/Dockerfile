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

# Base keeps the Agent Sandbox legacy runtime (uvicorn main:app on :8888)
# so the Python SDK can drive commands.run() against this sandbox. The
# kubectl-only path uses the same image with the runtime simply idle.
FROM us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/python-runtime-sandbox:latest-main

USER root

# Node 24 (opencode-ai requires Node >= 18) plus git/ripgrep that OpenCode
# shells out to for repo work.
RUN apt-get update \
  && apt-get install -y --no-install-recommends curl ca-certificates git ripgrep \
  && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
  && apt-get install -y --no-install-recommends nodejs \
  && rm -rf /var/lib/apt/lists/*

# Named non-root user matching the base image's UID 1000. OpenCode writes
# session and auth state under ~/.local/share/opencode, which the
# opencode-state PVC backs. The base image's UID 1000 is currently unnamed,
# so only create the account when absent.
RUN getent passwd 1000 >/dev/null || useradd -m -u 1000 python; \
  mkdir -p /workspace /home/python/.local/share/opencode \
  && chown -R 1000:1000 /workspace /home/python

RUN npm install -g opencode-ai@1.19.5

# The base image's CMD is "uvicorn main:app ..." which imports main from /app;
# WORKDIR moves to /workspace for agent work, so keep /app importable.
ENV PYTHONPATH=/app
ENV HOME=/home/python
WORKDIR /workspace

USER 1000
