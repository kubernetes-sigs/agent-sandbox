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
# The base must be recent: Ubuntu 24.04's bpftrace (0.20) generated BPF that
# the benchmark nodes' kernel verifier rejected for our larger probes
# (kretprobe:do_sys_openat2 with the path-bucket chain), silently costing
# runs of profiler data. 26.04 ships 0.25, which is past the version class
# (>=0.23) empirically proven to load on those kernels. Distro packages
# keep a single path for amd64/arm64 and avoid fetching large release
# artifacts from GitHub at build time (a 172MB AppImage download timed out
# in CI).
FROM ubuntu:26.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bpftrace \
      util-linux \
 && rm -rf /var/lib/apt/lists/*
