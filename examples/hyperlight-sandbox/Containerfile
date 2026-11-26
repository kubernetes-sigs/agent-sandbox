# The sandbox as an ordinary container. It holds hluk, the Hyperlight micro-VM
# monitor, and the guest rootfs it boots. It needs no container runtime beyond
# the node's, ships no kernel, since hluk embeds the Unikraft kernel, and runs
# without privilege. The pod only needs /dev/kvm, which the Hyperlight device
# plugin hands it. See the README.
#
# The image is just the static hluk release and the rootfs, with no shell or
# libc, so there is nothing in the container but the monitor. The two are
# pinned together here, because the driver inside the rootfs speaks the
# protocol of the hluk release the rootfs was built for.
#
# Build the rootfs first (`hluk build`, see the README), then:
#   cp .hluk/rootfs.cpio initrd.cpio
#   docker build -f Containerfile -t ${REPO}:hluk-v0.20.1 .
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
COPY initrd.cpio /guest/initrd.cpio
# The device plugin creates /dev/kvm for this user (its DEVICE_UID/DEVICE_GID,
# 65534 by default), and the template runs the pod as it.
USER 65534:65534
EXPOSE 8888
# --scratch-mb max gives the guest the pod's memory limit, less what hluk
# keeps for itself, so the pod names the guest's memory once, as its limit.
# The guest serves on 8888 on the pod's IP, may connect out where the
# template's NetworkPolicy allows, and resolves names as the pod does.
ENTRYPOINT ["/hluk", "run", "--initrd", "/guest/initrd.cpio", "--scratch-mb", "max", \
            "--net", "--port", "8888", "--resolv-conf", "/etc/resolv.conf", \
            "--env", "SANDBOX_BASE_DIR=/tmp/workspace", \
            "--guest-exec", "/app/main.py"]
