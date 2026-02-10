# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


import asyncio
import os
import logging
import random
from typing import AsyncIterator

import httpx
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import StreamingResponse

logger = logging.getLogger(__name__)
logging.basicConfig(
    level=logging.INFO,
    format='{"time": "%(asctime)s", "name": "%(name)s", "level": "%(levelname)s", "message": "%(message)s"}'
)

# Initialize K8s client
try:
    from kubernetes import client, config
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    k8s_v1 = client.CoreV1Api()
    K8S_AVAILABLE = True
except Exception as e:
    logger.warning(f"K8s client not available: {e}")
    k8s_v1 = None
    K8S_AVAILABLE = False

# Initialize the FastAPI application
app = FastAPI()

# Configuration
DEFAULT_SANDBOX_PORT = 8080  # Claude Fleet pods use 8080
DEFAULT_NAMESPACE = os.environ.get("KUBERNETES_NAMESPACE", "claude-fleet")
WARMPOOL_NAME = os.environ.get("WARMPOOL_NAME", "claude-fleet-pool")
http_client = httpx.AsyncClient(timeout=180.0)


def get_warm_pod_ips() -> list[str]:
    """Get IPs of all running warm pool pods (shuffled for load distribution)."""
    if not K8S_AVAILABLE or not k8s_v1:
        return []

    try:
        pods = k8s_v1.list_namespaced_pod(
            namespace=DEFAULT_NAMESPACE,
            field_selector="status.phase=Running"
        )

        pod_ips = []
        for pod in pods.items:
            if not pod.metadata.name.startswith(f"{WARMPOOL_NAME}-"):
                continue
            if pod.status.container_statuses:
                if all(c.ready for c in pod.status.container_statuses):
                    if pod.status.pod_ip:
                        pod_ips.append(pod.status.pod_ip)

        random.shuffle(pod_ips)
        return pod_ips
    except Exception as e:
        logger.error(f"Error querying warm pool: {e}")
        return []


@app.get("/healthz")
async def health_check():
    """A simple health check endpoint that always returns 200 OK."""
    return {"status": "ok"}


@app.api_route("/{full_path:path}", methods=['GET', 'POST', 'PUT', 'DELETE', 'PATCH'])
async def proxy_request(request: Request, full_path: str):
    """
    Receives all incoming requests and proxies to a warm pool pod.

    In stateless mode (no X-Sandbox-ID), picks any available warm pod.
    In legacy mode (with X-Sandbox-ID), routes to the specified pod service.
    """
    sandbox_id = request.headers.get("X-Sandbox-ID")
    namespace = request.headers.get("X-Sandbox-Namespace", DEFAULT_NAMESPACE)

    # Sanitize namespace to prevent DNS injection
    if not namespace.replace("-", "").isalnum():
        raise HTTPException(status_code=400, detail="Invalid namespace format.")

    try:
        port = int(request.headers.get("X-Sandbox-Port", DEFAULT_SANDBOX_PORT))
    except ValueError:
        raise HTTPException(status_code=400, detail="Invalid port format.")

    if sandbox_id:
        # Legacy mode: route to specific pod via service DNS
        target_hosts = [f"{sandbox_id}.{namespace}.svc.cluster.local"]
    else:
        # Stateless mode: get all warm pod IPs (shuffled)
        pod_ips = await asyncio.to_thread(get_warm_pod_ips)
        if not pod_ips:
            raise HTTPException(
                status_code=503,
                detail="No warm pods available. Service at capacity."
            )
        target_hosts = pod_ips

    headers = {
        key: value for (key, value) in request.headers.items()
        if key.lower() not in ('host', 'x-sandbox-id', 'x-sandbox-namespace', 'x-sandbox-port')
    }
    body = await request.body()

    # Try each pod until one accepts (not busy)
    for target_host in target_hosts:
        target_url = f"http://{target_host}:{port}/{full_path}"

        try:
            req = http_client.build_request(
                method=request.method,
                url=target_url,
                headers=headers,
                content=body,
            )
            resp = await http_client.send(req, stream=True)

            # If pod returns 503 (busy), try next pod
            if resp.status_code == 503 and target_host != target_hosts[-1]:
                await resp.aclose()
                logger.info(f"Pod {target_host} busy, trying next")
                continue

            logger.info(f"Routed to {target_host}/{full_path} -> {resp.status_code}")

            async def stream_and_close(response=resp) -> AsyncIterator[bytes]:
                """Stream response bytes, then close the httpx response."""
                try:
                    async for chunk in response.aiter_bytes():
                        yield chunk
                finally:
                    await response.aclose()

            return StreamingResponse(
                content=stream_and_close(),
                status_code=resp.status_code,
                headers=resp.headers,
            )
        except httpx.ConnectError as e:
            logger.error(f"Connection to {target_url} failed: {e}")
            # Try next pod if available
            if target_host != target_hosts[-1]:
                continue
            raise HTTPException(
                status_code=502,
                detail="Could not connect to backend sandbox",
            )
        except Exception as e:
            # Close the streaming response if it was opened
            if 'resp' in locals():
                await resp.aclose()
            logger.error(f"Unexpected proxy error: {e}")
            raise HTTPException(
                status_code=500,
                detail="An internal error occurred in the proxy.",
            )

    # All pods rejected (all busy)
    raise HTTPException(
        status_code=503,
        detail="All pods are busy. Service at capacity.",
    )
