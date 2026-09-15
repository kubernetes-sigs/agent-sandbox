# Builds Containarium's model-gateway image from source, so you don't have to
# trust the published ghcr.io/footprintai/containarium-model-gateway tag sight
# unseen — and so the example still works if that registry is unreachable from
# your cluster. Mirrors images/model-gateway/Dockerfile in
# https://github.com/FootprintAI/Containarium 1:1 (same build stage, same
# non-root runtime) except that the build stage clones the pinned release tag
# first, since this Dockerfile lives outside that repo and has no local source
# tree to use as build context.
#
#   docker build -t containarium-model-gateway:local .
#   kind load docker-image containarium-model-gateway:local --name execution-scoped-token
#
# Then point the manifest at your build instead of the published tag — either
# edit `image:` in gateway.yaml, or override it at apply time:
#
#   sed -E 's#ghcr.io/footprintai/containarium-model-gateway:v0.78.1@sha256:[0-9a-f]{64}#containarium-model-gateway:local#' \
#     gateway.yaml | kubectl apply -f -
#
# To build a different release, override the tag (it must exist as a git tag
# upstream, and must be >= v0.78.0, the first release carrying the admin revoke
# endpoint this example depends on):
#
#   docker build --build-arg CONTAINARIUM_REF=vX.Y.Z -t containarium-model-gateway:local .

ARG CONTAINARIUM_REF=v0.78.1

# --- fetch source at the pinned release tag, then build --------------------
# The gateway is a standalone credential proxy: it imports no Kubernetes
# client and needs no build tags, so this stays a small static binary. (The
# golang image ships git, needed to clone the tag.)
FROM golang:1.26.6-bookworm AS build
ARG CONTAINARIUM_REF
WORKDIR /src
RUN git clone --depth 1 --branch "${CONTAINARIUM_REF}" \
      https://github.com/FootprintAI/Containarium.git .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/model-gateway ./cmd/model-gateway

# --- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /usr/sbin/nologin model-gateway

COPY --from=build /out/model-gateway /usr/local/bin/model-gateway

USER model-gateway
ENV HOME=/home/model-gateway
# Documentation only; `serve --addr` binds this single port for both the model
# data plane and the /__gateway/* admin routes.
EXPOSE 8866
ENTRYPOINT ["/usr/local/bin/model-gateway"]
# Provider keys arrive by env and the HMAC secret / admin token by mounted
# file (see gateway.yaml) — never baked into the image.
CMD ["serve"]
