# Sandbox Router

The Sandbox Router is a lightweight, asynchronous reverse proxy designed to provide scalable and
dynamic access to thousands of ephemeral agent sandboxes running in a Kubernetes cluster. It acts as
a central entry point for all sandbox traffic, routing requests to the correct destination based on
a unique identifier.

## Architecture

This router is a key component of a scalable architecture that avoids the limitations of creating
individual network routes for every sandbox. Instead of having the Gateway route traffic directly to
each sandbox, all traffic is directed to a highly-available router deployment. This router then
forwards requests to the correct sandbox.

The request flow is as follows:

1. An external client sends a request to a single, static IP address provided by a Gateway. The
   request must include a header (e.g., `X-Sandbox-ID`) that specifies the unique name of the target
   sandbox.
2. A static `HTTPRoute` rule directs all incoming traffic from the Gateway to the `sandbox-router-svc`.
3. The router service load balances the request to one of the available router pods.
4. The router pod reads the `X-Sandbox-ID` header, constructs the internal Kubernetes DNS name for
   the target sandbox's headless service, and proxies the original request to it.
5. The response from the sandbox pod is streamed back along the same path to the original client.

## Building the Docker Image

The router is a Python application built with FastAPI and Uvicorn.

### Prerequisites

- Python 3.13+
- Docker

### Build Steps

Use the provided `Dockerfile` to build and push the image to your container registry.

```bash
export SANDBOX_ROUTER_IMG=your_registry_path/sandbox-router:latest
docker build -t $SANDBOX_ROUTER_IMG .
docker push $SANDBOX_ROUTER_IMG
```

## Deployment

### Deploy the Sandbox Router

The Sandbox Router (or similar reverse proxy service) is needed for both the "Gateway Mode" and
"Tunnel Mode" interactions with the Python client.

In `sandbox_router.yaml` replace `IMAGE_PLACEHOLDER` with the `$SANDBOX_ROUTER_IMG` from the
previous step, and then apply the manifest.

```bash
sed -i "s|IMAGE_PLACEHOLDER|${SANDBOX_ROUTER_IMG}|g" sandbox_router.yaml
kubectl apply -f sandbox_router.yaml
```

### Deploy the Gateway

In order to use the Python client in "Gateway Mode", you will need to create the Gateway resources.

Note that the example Gateway resources are specific to GKE. If running on a different Kubernetes
provider you will need to modify the `gateway.yaml`.

```bash
kubectl apply -f gateway.yaml
```
