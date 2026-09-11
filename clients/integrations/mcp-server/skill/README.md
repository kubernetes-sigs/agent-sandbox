# k8s-agent-sandbox-mcp

## Prerequisites
- A Kubernetes cluster.
- [k8s-agent-sandbox CRDs and Router installed](https://github.com/kubernetes-sigs/agent-sandbox).
- The [mcp-server](../README.md) deployed and reachable — see its deployment documentation for how to run it.
- Any sandbox warmpool deployed to the Kubernetes cluster.

## Setup
1. Deploy the mcp-server per the link above, and note its URL.
2. Install the `k8s-agent-sandbox-mcp` skill into your environment (consult your specific agent client's documentation for the exact installation procedure).
3. Edit `mcp-config.json` in this skill's directory, setting `mcpServers.k8s-agent-sandbox.url` to that URL.
