# Minimal image hosting an MCP server over streamable-http. Unlike
# mcp-server-sandbox (stdio, spawned per kubectl-exec session), this process
# runs continuously and is reached over the network via sandbox-router.
FROM python:3.11-slim

RUN pip install --no-cache-dir "mcp==1.27.2"

RUN install -d -m 0755 -o 1000 -g 1000 /app
COPY --chown=1000:1000 --chmod=644 mcp_server.py /app/mcp_server.py
WORKDIR /app

USER 1000:1000

EXPOSE 8000
CMD ["python3", "-u", "/app/mcp_server.py"]
