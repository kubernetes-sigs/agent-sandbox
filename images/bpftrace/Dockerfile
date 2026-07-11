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

# bpftrace for the stress-node-profiler DaemonSet (test/stress/profiler).
# util-linux provides nsenter for the ctr-events sidecar.
#
# bpftrace comes from the pinned upstream static release binary, NOT the
# Ubuntu package: 24.04 ships bpftrace 0.20, whose older codegen produces
# BPF programs that newer kernels' verifiers reject for our larger probes
# (kretprobe:do_sys_openat2 with the path-bucket chain loaded on a 6.1
# kernel but was rejected on the CI nodes' 6.8, silently costing runs of
# profiler data). Upstream publishes x86_64 only; arm64 falls back to the
# distro package (the benchmark nodes are amd64).
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ARG BPFTRACE_VERSION=v0.26.1

# The upstream release is a nix-based AppImage; FUSE is unavailable in
# containers, so extract it at build time and place the nix store at the
# image root, where the payload's absolute /nix/store paths resolve.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      util-linux \
      ca-certificates \
      curl \
 && if [ "${TARGETARCH}" = "amd64" ]; then \
      cd /tmp \
      && curl -fsSL -o bt.appimage \
        "https://github.com/bpftrace/bpftrace/releases/download/${BPFTRACE_VERSION}/bpftrace" \
      && chmod +x bt.appimage \
      && ./bt.appimage --appimage-extract >/dev/null \
      && cp -a squashfs-root/nix /nix \
      && ln -s /nix/store/*-bpftrace/bin/bpftrace /usr/local/bin/bpftrace \
      && rm -rf /tmp/bt.appimage /tmp/squashfs-root; \
    else \
      apt-get install -y --no-install-recommends bpftrace; \
    fi \
 && rm -rf /var/lib/apt/lists/*
