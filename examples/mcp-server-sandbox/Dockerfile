# Minimal image: just enough Python to host an MCP stdio server. Each
# `kubectl exec -i mcp-sandbox -- python3 -u /app/mcp_server.py` spawns
# a fresh server process whose stdin/stdout are wired through kubectl
# to a host-side MCP client.
FROM python:3.11-slim

RUN pip install --no-cache-dir "mcp==1.27.2"

# Create /app explicitly so its mode is deterministic (0755, traversable
# by uid 1000).
RUN install -d -m 0755 -o 1000 -g 1000 /app
COPY --chown=1000:1000 --chmod=644 mcp_server.py /app/mcp_server.py
WORKDIR /app

# Run as non-root. The matching sandbox.yaml sets fsGroup: 1000 so the
# /workspace PVC mount is writable by this UID.
USER 1000:1000

CMD ["sleep", "infinity"]
