# n8n with k8s-agent-sandbox MCP Server

## Overview

This guide explains how to set up n8n with an [MCP server](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/clients/integrations/mcp-server) that manages k8s-agent-sandbox instances.

## Set up a KinD cluster

Create a KinD cluster using this command:

```bash
kind create cluster --name agent-sandbox
```

Run the following commands to build and load a sandbox Docker image to the KinD cluster:

```bash
cd source
docker build -t local-sandbox:v1 .
kind load docker-image local-sandbox:v1 --name agent-sandbox
```

Install k8s-agent-sandbox CRDs and router using the [official guide](https://github.com/kubernetes-sigs/agent-sandbox#installation).

Apply these manifests to create `SandboxTemplate` and `SandboxWarmPool`:

```bash
kubectl apply -f template.yaml
kubectl apply -f warmpool.yaml
```

## Set up an MCP Server

Go to the `clients/integrations/mcp-server` directory and create `.env` file with this content:

```.env
K8S_SANDBOX_CONNECTION__TYPE=direct
K8S_SANDBOX_CONNECTION__API_URL=127.0.0.1
K8S_SANDBOX_CONNECTION__SERVER_PORT=8888
```

Create a new Python environment, install the dependencies and run the server:

```bash
python -m venv .venv
. .venv/bin/activate
pip install --no-cache-dir .
uvicorn k8s_agent_sandbox_mcp_server.app:app --host 0.0.0.0 --port 8000
```

## Run n8n in a Docker container

In a new terminal run these commands to run n8n in a Docker container:

```bash
docker volume create n8n_data

docker run -it --rm \
 --name n8n \
 -p 5678:5678 \
 -e GENERIC_TIMEZONE="Etc/UTC" \
 -e TZ="Etc/UTC" \
 -e N8N_ENFORCE_SETTINGS_FILE_PERMISSIONS=true \
 -e N8N_RUNNERS_ENABLED=true \
 -v n8n_data:/home/node/.n8n \
 docker.n8n.io/n8nio/n8n \
 --add-host=host.docker.internal:host-gateway
```

Open `http://127.0.0.1:5678` and click the `Add first step...` button. Search for `MCP Client` and select it. In the `MCP Client` configuration set `MCP Endpoint URL` to `http://host.docker.internal:8000/mcp`. The modal window will acquire the specification and you will be able to set up everything from dropdowns. For example:

- `Tool`: `create_sandbox`
- `warmpool`: `simple-sandbox-warmpool`
- `namespace`: `default`

Hit the `Execute step` button and run in the terminal the following command:

```bash
kubectl get sandboxclaim
```

The output should look like this:

```log
NAME                     AGE
sandbox-claim-106ba612   3s
```

## Clean up

```bash
kind delete cluster --name agent-sandbox
docker stop n8n
docker volume rm n8n_data
docker rmi docker.n8n.io/n8nio/n8n
```
