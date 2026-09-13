# Copyright 2026 The Kubernetes Authors.
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

"""Kubernetes liveness and readiness endpoints.

The two probes answer deliberately different questions, following the split
`sandbox-router` uses (`sandbox-router/server/server.go`):

* ``/healthz`` — is the process alive and serving HTTP? Never touches the
  Kubernetes API, so an apiserver outage cannot cause a restart loop. If this
  fails the process is wedged and restarting is the right response.
* ``/readyz`` — can this replica actually serve tool calls? Every tool resolves
  a SandboxClaim first, so this checks the Kubernetes API is reachable. If it
  fails the pod is pulled from the Service while the process keeps running.

Keeping ``/healthz`` free of the apiserver is the important part: wiring the
liveness probe to an external dependency turns a transient control-plane blip
into a rolling restart of every replica, which is strictly worse than briefly
serving no traffic.
"""

import logging

from starlette.requests import Request
from starlette.responses import JSONResponse, PlainTextResponse


logger = logging.getLogger(__name__)


async def healthz(request: Request) -> PlainTextResponse:
    """Liveness: the process is up and the ASGI app is routing requests.

    Deliberately dependency-free -- reaching this handler at all is the signal.
    """
    return PlainTextResponse("ok")


async def readyz(request: Request) -> JSONResponse:
    """Readiness: the Kubernetes client is usable, so tool calls can be served.

    Probes ``list_sandbox_claims`` in the configured probe namespace. That is
    a public SDK call (the private ``_ensure_initialized`` was avoided
    deliberately -- nothing else in the repo depends on SDK internals) and it
    exercises the whole path a tool call needs: credential loading, client
    construction, and one real apiserver request.

    A LIST bounded to a single namespace is the cheapest call that proves all
    three. It is not free, so keep the probe's ``periodSeconds`` modest rather
    than polling it aggressively.
    """
    server = getattr(request.app.state, "fastmcp_server", None)
    client = getattr(server, "sandbox_client", None)
    settings = getattr(server, "probe_settings", None)
    if client is None or settings is None:
        # The lifespan has not run yet: still starting, so not ready -- but not
        # unhealthy, which is why /healthz stays independent of this.
        return JSONResponse({"ready": False, "reason": "starting"}, status_code=503)

    try:
        await client.k8s_helper.list_sandbox_claims(settings.probe_namespace)
    except Exception as e:
        logger.warning("readiness check failed: %s", e)
        return JSONResponse(
            {"ready": False, "reason": "kubernetes API unavailable"},
            status_code=503,
        )

    return JSONResponse({"ready": True})
