# Minimal image: just enough Python to host an MCP stdio server. Each
# `kubectl exec -i mcp-sandbox -- python3 -u /app/mcp_server.py` spawns
# a fresh server process whose stdin/stdout are wired through kubectl
# to a host-side MCP client.
FROM python:3.11-slim

RUN pip install --no-cache-dir "mcp>=1.0.0"

COPY mcp_server.py /app/mcp_server.py
WORKDIR /app

CMD ["sleep", "infinity"]
