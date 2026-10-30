# The sandbox as an ordinary container, resumed rather than booted. It holds
# hluk and a saved snapshot of the guest, taken after the guest booted and
# imported what the runtime uses, right before it runs anything. `hluk
# snapshot run` restores the snapshot in tens of milliseconds, so a warm
# pool's replacement serves almost as soon as it is scheduled.
#
# Save the snapshot first (see the README), then:
#   docker build -f Containerfile.snapshot -t ${REPO}:hluk-v0.20.1-snapshot .
ARG HLUK_VERSION=v0.20.1

FROM debian:12-slim AS fetch
ARG HLUK_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL https://raw.githubusercontent.com/hyperlight-dev/hyperlight-unikraft/${HLUK_VERSION}/install.sh \
    | HLUK_VERSION=${HLUK_VERSION} HLUK_INSTALL_DIR=/out sh \
 && /out/hluk --version

FROM scratch
COPY --from=fetch /out/hluk /hluk
# hluk saves a snapshot's files for its owner only, and the container's user
# has to be able to read them.
COPY --chown=65534:65534 snapshot /guest/snapshot
USER 65534:65534
EXPOSE 8888
# A snapshot keeps the memory size it was saved with, 1536 MiB here, so the
# pod's limit has to hold that plus what hluk keeps for itself. hluk says so
# on stderr when it does not. The restore takes the network, the resolver and
# the environment as a boot does.
ENTRYPOINT ["/hluk", "snapshot", "run", "/guest/snapshot", \
            "--net", "--port", "8888", "--resolv-conf", "/etc/resolv.conf", \
            "--env", "SANDBOX_BASE_DIR=/tmp/workspace", \
            "--guest-exec", "/app/main.py"]
