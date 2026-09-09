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

# Draft runtime image for the agent-sandbox `examples/nono-sandbox` example.
# Bundles the nono binary and a tiny demo workload that proves in-pod enforcement.
#
# nono runs fully unprivileged (no CAP_SYS_ADMIN). The image runs as a non-root
# user; the pod spec drops ALL capabilities. See README for the Landlock/runtime
# prerequisite.

FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS base

ARG TARGETARCH

# --- install nono ---------------------------------------------------------
# Pin a released version and verify the release artifact. For an air-gapped build,
# COPY the same checksummed artifact instead of downloading it.
ARG NONO_VERSION=0.71.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl python3 tar \
 && rm -rf /var/lib/apt/lists/* \
 && arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "${arch}" in \
      amd64) nono_arch="x86_64"; nono_sha256="9ee2966184e8afa664199c21e089eae596c8d763371a253104623e0f5b0f0a2d" ;; \
      arm64) nono_arch="aarch64"; nono_sha256="f35f979dab33d604be442304f1945f60bfd7a0e6a5df7dd8c3d43abdcdf921a1" ;; \
      *) echo "unsupported nono architecture: ${arch}" >&2; exit 1 ;; \
    esac \
 && curl -fsSL \
      "https://github.com/nolabs-ai/nono/releases/download/v${NONO_VERSION}/nono-v${NONO_VERSION}-${nono_arch}-unknown-linux-gnu.tar.gz" \
      -o /tmp/nono.tar.gz \
 && echo "${nono_sha256}  /tmp/nono.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/nono.tar.gz -C /tmp \
 && install -m 0755 /tmp/nono /usr/local/bin/nono \
 && rm -f /tmp/nono /tmp/nono.tar.gz \
 && nono --version

# Pin LogCLI to the same release as the demo Loki server. Verify Grafana's
# published release artifact so the example works reproducibly on Intel and Arm
# hosts (including Apple Silicon building a Linux/arm64 image).
ARG LOGCLI_VERSION=3.7.2
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
 && case "${arch}" in \
      amd64) logcli_sha256="7850d566d2af10d7adf255ed9452de632ab20c0f269dc61fac7f70bed4d99e48" ;; \
      arm64) logcli_sha256="2fec7cbf4c0929f2fbd1e339753b14bc6432aadb41abf4b4155b00d3f6509e4e" ;; \
      *) echo "unsupported LogCLI architecture: ${arch}" >&2; exit 1 ;; \
    esac \
 && curl -fsSL \
      "https://github.com/grafana/loki/releases/download/v${LOGCLI_VERSION}/logcli-linux-${arch}.zip" \
      -o /tmp/logcli.zip \
 && echo "${logcli_sha256}  /tmp/logcli.zip" | sha256sum -c - \
 && python3 -m zipfile -e /tmp/logcli.zip /tmp/logcli \
 && install -m 0755 /tmp/logcli/logcli-linux-${arch} /usr/local/bin/logcli \
 && rm -rf /tmp/logcli /tmp/logcli.zip \
 && logcli --version

# --- demo workload ---------------------------------------------------------
# A trivial script that tries permitted and denied filesystem, network, and
# per-tool operations, so the logs make each sandbox boundary visible.
COPY workload.py /opt/agent/workload.py

# Non-root, matches the securityContext in the Sandbox manifest.
RUN useradd --uid 1000 --create-home --home-dir /home/agent agent
USER 1000
WORKDIR /workspace

# The entrypoint is nono; the actual command is supplied by the Sandbox manifest
# so the profile can be selected per-deployment.
ENTRYPOINT ["/usr/local/bin/nono"]
