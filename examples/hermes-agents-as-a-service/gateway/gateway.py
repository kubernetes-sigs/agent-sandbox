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

"""Minimal agents-as-a-service gateway.

A compact, single-file version of the control+data plane a real platform
puts in front of agent-sandbox (distilled from
https://github.com/aditya-shantanu/ai-agent-service):

  POST   /users {"user": "alice"}  signup: create a SandboxClaim over the
                                   warm pool, wait Ready, mint a bearer
                                   token (only its SHA-256 is stored, as a
                                   claim annotation)
  GET    /users/<user>             derived state: Ready/Waking/Suspended/...
                                   (requires the user's bearer token)
  DELETE /users/<user>             delete the claim (cascades sandbox + PVC;
                                   requires the user's bearer token)
  ANY    /u/<user>/<path>          per-user proxy. Requests to a suspended
                                   agent are HELD while it resumes
                                   (wake-on-connect); /u/<user>/v1/* goes to
                                   the OpenAI-compatible API on :8642 (the
                                   platform API key is injected upstream),
                                   everything else to the dashboard on :9119

A background sweeper suspends agents idle for IDLE_TIMEOUT seconds —
suspend deletes only the pod; the PVC (and the user's state) survives.

Deliberately NOT production code: single replica, in-memory idle clock,
no TLS. The linked platform adds adaptive idle windows, cron-aware wakes,
gVisor, and benchmarks on top of exactly this resource model.
"""

import hashlib
import os
import re
import secrets
import threading
import time

import requests
from flask import Flask, Response, jsonify, request
from kubernetes import client, config

GROUP, VERSION = "extensions.agents.x-k8s.io", "v1beta1"
CORE_GROUP = "agents.x-k8s.io"
NAMESPACE = os.environ.get("NAMESPACE", "hermes-demo")
POOL = os.environ.get("POOL", "hermes-pool")
API_SERVER_KEY = os.environ["API_SERVER_KEY"]  # injected upstream on /v1/*
IDLE_TIMEOUT = int(os.environ.get("IDLE_TIMEOUT", "60"))
WAKE_TIMEOUT = int(os.environ.get("WAKE_TIMEOUT", "120"))
TOKEN_ANNOTATION = "aaas.example.com/token-sha256"
# RFC 1123 label: lowercase alphanumerics and '-', must start/end
# alphanumeric. The claim name is "hermes-<user>", so cap the user part at
# 63 - len("hermes-") to keep the claim a valid Kubernetes object name.
DNS1123_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
MAX_USER_LEN = 63 - len("hermes-")

try:
    config.load_incluster_config()
except config.ConfigException:
    config.load_kube_config()
crd = client.CustomObjectsApi()

app = Flask(__name__)
last_activity: dict[str, float] = {}


def claim_name(user: str) -> str:
    return f"hermes-{user}"


def get_claim(user: str):
    try:
        return crd.get_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims", claim_name(user))
    except client.ApiException as e:
        if e.status == 404:
            return None
        raise


def get_sandbox(name: str):
    return crd.get_namespaced_custom_object(
        CORE_GROUP, VERSION, NAMESPACE, "sandboxes", name)


def sandbox_of(claim):
    name = (claim.get("status") or {}).get("sandbox", {}).get("name")
    if not name:
        return None
    try:
        return get_sandbox(name)
    except client.ApiException as e:
        if e.status == 404:  # stale claim status, e.g. mid-cascade-delete
            return None
        raise


def condition(obj, ctype: str) -> bool:
    for c in (obj.get("status") or {}).get("conditions", []):
        if c.get("type") == ctype:
            return c.get("status") == "True"
    return False


def derive_state(sandbox) -> str:
    """The state machine a platform shows users (mode x conditions)."""
    if sandbox is None:
        return "Provisioning"
    mode = sandbox["spec"].get("operatingMode", "Running")
    if mode == "Suspended":
        return "Suspended" if condition(sandbox, "Suspended") else "Suspending"
    return "Ready" if condition(sandbox, "Ready") else "Waking"


def set_operating_mode(sandbox_name: str, mode: str):
    crd.patch_namespaced_custom_object(
        CORE_GROUP, VERSION, NAMESPACE, "sandboxes", sandbox_name,
        {"spec": {"operatingMode": mode}})


def wait_ready(user: str, timeout: int):
    deadline = time.time() + timeout
    while time.time() < deadline:
        claim = get_claim(user)
        sandbox = sandbox_of(claim) if claim else None
        if sandbox is not None and derive_state(sandbox) == "Ready":
            return sandbox
        time.sleep(1)
    return None


def authorized(claim) -> bool:
    # Header-only on purpose: a ?token= query param would leak the bearer
    # token into access logs, browser history and Referer headers.
    token = request.headers.get("Authorization", "").removeprefix("Bearer ")
    want = (claim["metadata"].get("annotations") or {}).get(TOKEN_ANNOTATION, "")
    got = hashlib.sha256(token.encode()).hexdigest()
    return bool(token) and secrets.compare_digest(got, want)


@app.post("/users")
def create_user():
    user = (request.get_json(silent=True) or {}).get("user", "")
    if not isinstance(user, str) or not DNS1123_LABEL.fullmatch(user) \
            or len(user) > MAX_USER_LEN:
        return jsonify(error="body must be {'user': '<dns-1123 label>'}"), 400
    if get_claim(user) is not None:
        return jsonify(error="user exists"), 409
    token = secrets.token_urlsafe(32)
    # The claim IS the user record: warmPoolRef only (env/VCTs would be
    # rejected by the template policies), token hash as an annotation.
    try:
        crd.create_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims", {
                "apiVersion": f"{GROUP}/{VERSION}",
                "kind": "SandboxClaim",
                "metadata": {
                    "name": claim_name(user),
                    "annotations": {
                        TOKEN_ANNOTATION: hashlib.sha256(token.encode()).hexdigest()},
                },
                "spec": {
                    "warmPoolRef": {"name": POOL},
                    "additionalPodMetadata": {
                        "labels": {"sandbox.users.io/hermes-user": user}},
                },
            })
    except client.ApiException as e:
        if e.status == 409:  # lost a concurrent-signup race for this user
            return jsonify(error="user exists"), 409
        raise
    sandbox = wait_ready(user, WAKE_TIMEOUT)
    last_activity[user] = time.time()
    return jsonify(
        user=user,
        token=token,  # shown once; only the hash is stored
        state=derive_state(sandbox),
        sandbox=(sandbox or {}).get("metadata", {}).get("name"),
    ), 201


@app.get("/users/<user>")
def get_user(user):
    claim = get_claim(user)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):  # only the token holder may read their state
        return jsonify(error="unauthorized"), 401
    sandbox = sandbox_of(claim)
    return jsonify(user=user, state=derive_state(sandbox),
                   sandbox=(sandbox or {}).get("metadata", {}).get("name"))


@app.delete("/users/<user>")
def delete_user(user):
    claim = get_claim(user)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):  # deletion cascades the PVC: token required
        return jsonify(error="unauthorized"), 401
    try:
        crd.delete_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims", claim_name(user))
    except client.ApiException as e:
        if e.status != 404:  # already gone => idempotent success
            raise
    last_activity.pop(user, None)
    return jsonify(user=user, note="claim deleted; sandbox and PVC cascade")


@app.route("/u/<user>/", defaults={"path": ""},
           methods=["GET", "POST", "PUT", "DELETE", "PATCH"])
@app.route("/u/<user>/<path:path>",
           methods=["GET", "POST", "PUT", "DELETE", "PATCH"])
def proxy(user, path):
    claim = get_claim(user)
    if claim is None:
        return jsonify(error="unknown user"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    last_activity[user] = time.time()

    sandbox = sandbox_of(claim)
    if sandbox is None:
        return jsonify(error="sandbox not provisioned yet"), 503
    if derive_state(sandbox) != "Ready":
        # Wake-on-connect: flip the mode (safe in ANY state, including
        # mid-suspension) and HOLD the request until Ready.
        set_operating_mode(sandbox["metadata"]["name"], "Running")
        sandbox = wait_ready(user, WAKE_TIMEOUT)
        if sandbox is None:
            return jsonify(error="agent is waking up, retry shortly"), 503, \
                {"Retry-After": "10"}
        last_activity[user] = time.time()

    fqdn = (sandbox.get("status") or {}).get("serviceFQDN")
    headers = {k: v for k, v in request.headers
               if k.lower() not in ("host", "authorization")}
    # Auth is header-only, but never forward a stray ?token= upstream where
    # the app (or its logs) would see it.
    params = [(k, v) for k, v in request.args.items(multi=True)
              if k != "token"]
    if path == "v1" or path.startswith("v1/"):
        upstream = f"http://{fqdn}:8642/{path}"
        headers["Authorization"] = f"Bearer {API_SERVER_KEY}"
    else:
        upstream = f"http://{fqdn}:9119/{path}"

    # The pod can be Ready a beat before the server binds; retry the dial.
    for attempt in range(5):
        try:
            r = requests.request(
                request.method, upstream, params=params,
                data=request.get_data(), headers=headers,
                stream=True, timeout=300)
            break
        except requests.ConnectionError:
            if attempt == 4:
                return jsonify(error="upstream unavailable"), 502
            time.sleep(1)
    return Response(
        r.iter_content(chunk_size=None),
        status=r.status_code,
        headers=[(k, v) for k, v in r.headers.items()
                 if k.lower() not in ("transfer-encoding", "connection")])


def idle_sweeper():
    """Suspend agents idle for IDLE_TIMEOUT (pod deleted, PVC survives)."""
    while True:
        time.sleep(15)
        claims = crd.list_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims")["items"]
        for c in claims:
            user = c["metadata"]["name"].removeprefix("hermes-")
            try:
                sandbox = sandbox_of(c)
            except client.ApiException:
                continue
            if sandbox is None or derive_state(sandbox) != "Ready":
                continue
            idle = time.time() - last_activity.setdefault(user, time.time())
            if idle >= IDLE_TIMEOUT:
                print(f"suspending {user} (idle {int(idle)}s)", flush=True)
                set_operating_mode(sandbox["metadata"]["name"], "Suspended")


if __name__ == "__main__":
    threading.Thread(target=idle_sweeper, daemon=True).start()
    # threaded=True so one user's wake-on-connect (which can hold a request
    # for up to WAKE_TIMEOUT) doesn't block everyone else. Still Werkzeug's
    # dev server — a real deployment would sit behind gunicorn or similar.
    app.run(host="0.0.0.0", port=8080, threaded=True)
