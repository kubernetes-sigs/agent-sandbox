# Tiny HTTP "exec" sandbox image for the ingress demo: stdlib server + git.
# The server exposes /healthz and POST /execute on port 8888 (the agent-sandbox
# default sandbox port the router proxies to). git + ca-certificates let it clone
# over HTTPS — subject to the pod's CiliumNetworkPolicy egress rules.
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY server.py .

USER 1000:1000
EXPOSE 8888
CMD ["python", "server.py"]
