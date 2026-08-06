# Squid built against OpenSSL, with the cert-generation helpers needed for
# `ssl_bump` (TLS interception). Canonical's `ubuntu/squid` image is built
# against GnuTLS and does NOT ship `security_file_certgen`, so it cannot do
# ssl_bump — see README §7.5 for the diagnosis.
#
# Build and push to a registry your cluster can pull from:
#
#   IMAGE=gcr.io/PROJECT_ID/agent-sandbox-egress-gateway:1
#   docker build -t "$IMAGE" examples/policy/cilium/gateway/
#   docker push "$IMAGE"
#
# Then update `egress-gateway.yaml` to reference $IMAGE in both the
# `init-ssl-db` initContainer and the main `squid` container.
FROM ubuntu:24.04

# squid-openssl: Squid built with --with-openssl, includes the
#   security_file_certgen helper at /usr/lib/squid/security_file_certgen.
# ca-certificates: standard system trust store for outbound TLS to upstreams.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      squid-openssl \
      ca-certificates \
      curl \
 && rm -rf /var/lib/apt/lists/*
# Leave /var/log/squid/{access,cache}.log alone — the squid-openssl package
# creates them with `proxy:proxy` ownership at install time. Deleting them
# here would force Squid to recreate them at first write, which can produce
# the wrong ownership after Squid drops privileges to the proxy user.

# Run as PID 1 in foreground; Squid then drops privileges to `proxy` (UID 13)
# itself per the package's cache_effective_user default.
# -N: do not daemonize.
# -f: config path (matches the ConfigMap mount in egress-gateway.yaml).
EXPOSE 3128
CMD ["squid", "-N", "-f", "/etc/squid/squid.conf"]
