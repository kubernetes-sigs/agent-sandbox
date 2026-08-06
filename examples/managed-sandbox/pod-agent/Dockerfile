# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# syntax=docker/dockerfile:1.7

# ---------- builder ----------
FROM docker.io/library/rust:1.95.0-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends \
        protobuf-compiler \
        musl-tools \
    && rm -rf /var/lib/apt/lists/*

# The worker binary gets bind-mounted into the user's OCI image inside
# bwrap, so it must be a fully static executable — alpine/busybox/etc.
# have no glibc loader. Pod-agent stays on the host's libc since we
# control its image (debian-slim below).
ARG MUSL_TARGET=x86_64-unknown-linux-musl
RUN rustup target add ${MUSL_TARGET}
WORKDIR /src

# Copy the proto tree first so build.rs has a stable view.
COPY proto/ /src/proto/
COPY pod-agent/ /src/pod-agent/

WORKDIR /src/pod-agent
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/pod-agent/target \
    cargo build --release --bin pod-agent && \
    cargo build --release --target ${MUSL_TARGET} --bin agent-sandbox-worker && \
    cp target/release/pod-agent /usr/local/bin/pod-agent && \
    cp target/${MUSL_TARGET}/release/agent-sandbox-worker /usr/local/bin/agent-sandbox-worker

# ---------- runtime ----------
FROM docker.io/library/debian:bookworm-slim
# bwrap, overlay tooling, networking. uidmap for hostUsers=false.
RUN apt-get update && apt-get install -y --no-install-recommends \
        bubblewrap \
        uidmap \
        iproute2 \
        nftables \
        ca-certificates \
        e2fsprogs \
        wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/pod-agent /usr/local/bin/pod-agent
COPY --from=builder /usr/local/bin/agent-sandbox-worker /usr/local/bin/agent-sandbox-worker

ENTRYPOINT ["/usr/local/bin/pod-agent"]
