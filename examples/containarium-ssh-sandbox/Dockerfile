# Builds Containarium's agent-box image from source, so you don't have to
# trust the published ghcr.io/footprintai/containarium-agent-box tag sight
# unseen. Mirrors images/agent-box/Dockerfile in
# https://github.com/FootprintAI/Containarium 1:1 — same build stage, same
# runtime hardening (rootless dropbear, forced command) — except the build
# stage clones the pinned release tag first, since this Dockerfile lives
# outside that repo and has no local source tree to use as build context.
#
#   docker build -t containarium-agent-box:local .
#   export AGENT_BOX_IMAGE=containarium-agent-box:local
#   kind load docker-image "${AGENT_BOX_IMAGE}" --name <your-kind-cluster>
#
# To build a different release, override the tag (must exist as a git tag in
# the upstream repo):
#   docker build --build-arg CONTAINARIUM_REF=v0.53.0 -t containarium-agent-box:local .
#
# See the README's "Building agent-box from source" section for how
# AGENT_BOX_IMAGE feeds into the manifests in this directory.

ARG CONTAINARIUM_REF=v0.52.1

# --- fetch source at the pinned release tag, then build --------------------
# No `k8s` build tag: agent-box is the in-box MCP and never imports
# client-go, so this stays a small static binary. (golang's own image
# ships git, needed to clone the tag.)
FROM golang:1.26.5-bookworm AS build
ARG CONTAINARIUM_REF
WORKDIR /src
RUN git clone --depth 1 --branch "${CONTAINARIUM_REF}" \
      https://github.com/FootprintAI/Containarium.git .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent-box ./cmd/agent-box

# --- runtime: rootless dropbear + agent-box --------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends dropbear-bin ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash agent \
    && mkdir -p /home/agent/.ssh /etc/agent-box \
    && chown -R agent:agent /home/agent /etc/agent-box

COPY --from=build /out/agent-box /usr/local/bin/agent-box
COPY --from=build /src/images/agent-box/entrypoint.sh /usr/local/bin/agent-box-entrypoint.sh
RUN chmod 0755 /usr/local/bin/agent-box-entrypoint.sh

USER agent
ENV HOME=/home/agent
# Documentation only; the entrypoint binds this port.
EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/agent-box-entrypoint.sh"]
