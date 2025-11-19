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


import httpx
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import StreamingResponse

# Initialize the FastAPI application
app = FastAPI()

# Configuration
DEFAULT_SANDBOX_PORT = 8888
DEFAULT_NAMESPACE = "default"
client = httpx.AsyncClient(timeout=180.0)


@app.get("/healthz")
async def health_check():
    """A simple health check endpoint that always returns 200 OK."""
    return {"status": "ok"}


@app.api_route("/{full_path:path}", methods=['GET', 'POST', 'PUT', 'DELETE', 'PATCH'])
async def proxy_request(request: Request, full_path: str):
    """
    Receives all incoming requests, determines the target sandbox from headers,
    and asynchronously proxies the request to it.
    """
    sandbox_id = request.headers.get("X-Sandbox-ID")
    if not sandbox_id:
        raise HTTPException(
            status_code=400, detail="X-Sandbox-ID header is required.")

    # Dynamic discovery via headers
    namespace = request.headers.get("X-Sandbox-Namespace", DEFAULT_NAMESPACE)
    
    # Sanitize namespace to prevent DNS injection
    if not namespace.replace("-", "").isalnum():
        raise HTTPException(status_code=400, detail="Invalid namespace format.")

    try:
        port = int(request.headers.get("X-Sandbox-Port", DEFAULT_SANDBOX_PORT))
    except ValueError:
        raise HTTPException(status_code=400, detail="Invalid port format.")

    # Construct the K8s internal DNS name
    target_host = f"{sandbox_id}.{namespace}.svc.cluster.local"
    target_url = f"http://{target_host}:{port}/{full_path}"

    print(f"Proxying request for sandbox '{sandbox_id}' to URL: {target_url}")

    try:
        headers = {key: value for (
            key, value) in request.headers.items() if key.lower() != 'host'}

        req = client.build_request(
            method=request.method,
            url=target_url,
            headers=headers,
            content=await request.body()
        )

        resp = await client.send(req, stream=True)

        return StreamingResponse(
            content=resp.aiter_bytes(),
            status_code=resp.status_code,
            headers=resp.headers
        )
    except httpx.ConnectError as e:
        print(
            f"ERROR: Connection to sandbox at {target_url} failed. Error: {e}")
        raise HTTPException(
            status_code=502, detail=f"Could not connect to the backend sandbox: {sandbox_id}")
    except Exception as e:
        print(f"An unexpected error occurred: {e}")
        raise HTTPException(
            status_code=500, detail="An internal error occurred in the proxy.")