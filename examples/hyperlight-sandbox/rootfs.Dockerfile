# The guest's root filesystem. It is the published hyperlight-unikraft `agent`
# rootfs, with CPython 3.12, a BusyBox shell, numpy, pandas, scipy and
# scikit-learn, plus the sandbox runtime at /app/main.py. `hluk build` builds
# it with Docker and converts it to the CPIO the guest boots from, at
# .hluk/rootfs.cpio.
#
# The tag pins the hluk release. The runtime driver inside the rootfs speaks
# that release's protocol, so it has to match the hluk that runs it.
FROM ghcr.io/hyperlight-dev/hyperlight-unikraft/agent:v0.20.1
COPY main.py /app/main.py
# The agent rootfs carries Debian's CA bundle, but its OpenSSL looks for one
# at /usr/lib/ssl/cert.pem, so HTTPS from the guest fails to verify until the
# bundle is reachable there.
RUN mkdir -p /usr/lib/ssl && ln -s /etc/ssl/certs/ca-certificates.crt /usr/lib/ssl/cert.pem
