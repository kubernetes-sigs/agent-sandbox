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

"""OpenClaw fleet portal: the per-employee control plane.

Adapted from ../../hermes-agents-as-a-service/gateway/gateway.py, extended
with the two things a late-binding fleet needs: triggering the storage node
daemon (10-storage.yaml) after every pod (re)creation, and maintaining a
stable per-employee address (an ExternalName Service alias, consumed by the
sandbox-router's path routing).

  POST   /employees {"employee": "emp-12345"}
                               provision: create a SandboxClaim over the warm
                               pool, bind the employee's Filestore workspace
                               into the adopted pod, alias oc-<employee> to
                               the sandbox's stable DNS name, mint a bearer
                               token (only its SHA-256 is stored, as a claim
                               annotation). The response carries a timing
                               breakdown (adopted/bound/app-ready ms) — this
                               is the measurement source for test-checklist
                               item 1 (expected vs. actual startup time).
  GET    /employees/<employee> derived state: Ready/Waking/Suspended/...
  POST   /employees/<employee>/suspend
                               sleep: pod is deleted, Service + workspace
                               survive (test-checklist item 3)
  POST   /employees/<employee>/wake
                               resume: pod recreated, workspace RE-BOUND
                               (every new pod needs a fresh bind), OpenClaw
                               restarts from persisted state; reports wake_ms
  POST   /employees/<employee>/rebuild
                               rolling update primitive (test-checklist item
                               4): delete the claim, re-claim from the (by
                               then refreshed) warm pool, re-bind, re-alias;
                               reports per-employee downtime_ms
  DELETE /employees/<employee>[?purge=true]
                               delete claim + alias; purge=true also deletes
                               the Filestore workspace (item 2)

Deliberately NOT production code: single replica, in-memory idle clock, no
TLS. A real fleet manager adds a durable job queue, rate-limited bulk
operations and audit logging on exactly this resource model.
"""

import hashlib
import hmac
import os
import re
import secrets
import threading
import time

import requests
from flask import Flask, jsonify, request
from kubernetes import client, config

GROUP, VERSION = "extensions.agents.x-k8s.io", "v1beta1"
CORE_GROUP = "agents.x-k8s.io"
NAMESPACE = os.environ.get("NAMESPACE", "openclaw-fleet")
POOL = os.environ.get("POOL", "openclaw-fleet-pool")
DAEMON_TOKEN = os.environ["DAEMON_TOKEN"]
ADMIN_TOKEN_SHA256 = os.environ.get("ADMIN_TOKEN_SHA256", "")
OPENCLAW_PORT = int(os.environ.get("OPENCLAW_PORT", "18789"))
# The emptyDir volume name and in-volume subdirectory the storage daemon
# binds. Must match 20-openclaw-template.yaml ('workspace-volume', HOME
# /workspace => OpenClaw state dir /workspace/.openclaw).
VOLUME_NAME = os.environ.get("VOLUME_NAME", "workspace-volume")
SUB_DIR = os.environ.get("SUB_DIR", ".openclaw")
WAKE_TIMEOUT = int(os.environ.get("WAKE_TIMEOUT", "180"))
IDLE_TIMEOUT = int(os.environ.get("IDLE_TIMEOUT", "0"))  # 0 = sweeper off
TOKEN_ANNOTATION = "fleet.example.com/token-sha256"
EMPLOYEE_LABEL = "sandbox.users.io/employee"
DNS1123_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
MAX_EMPLOYEE_LEN = 63 - len("oc-")

try:
    config.load_incluster_config()
except config.ConfigException:
    config.load_kube_config()
crd = client.CustomObjectsApi()
core = client.CoreV1Api()

app = Flask(__name__)
last_activity: dict[str, float] = {}


def claim_name(employee: str) -> str:
    return f"oc-{employee}"


def get_claim(employee: str):
    try:
        return crd.get_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims", claim_name(employee))
    except client.ApiException as e:
        if e.status == 404:
            return None
        raise


def sandbox_of(claim):
    name = (claim.get("status") or {}).get("sandbox", {}).get("name")
    if not name:
        return None
    try:
        return crd.get_namespaced_custom_object(
            CORE_GROUP, VERSION, NAMESPACE, "sandboxes", name)
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


def pod_of(employee: str, not_before: float = 0.0):
    """The employee's sandbox pod (the claim label is stamped at adoption).

    not_before filters out a terminating predecessor during wake/rebuild:
    only pods created after that wall-clock instant qualify.
    """
    pods = core.list_namespaced_pod(
        NAMESPACE, label_selector=f"{EMPLOYEE_LABEL}={employee}").items
    live = [p for p in pods if p.metadata.deletion_timestamp is None
            and p.metadata.creation_timestamp.timestamp() >= not_before]
    return live[0] if live else None


def daemon_bind(pod, employee: str, action: str = "bind"):
    """Ask the storage daemon on the pod's node to (un)bind the workspace.

    Retries: right after pod creation the emptyDir may not exist on the
    host yet (kubelet sets up volumes as the pod starts), so 500s are
    expected for a beat.
    """
    daemons = core.list_namespaced_pod(
        NAMESPACE, label_selector="app=storage-node-daemon",
        field_selector=f"spec.nodeName={pod.spec.node_name}").items
    if not daemons or not daemons[0].status.pod_ip:
        raise RuntimeError(f"no storage daemon on node {pod.spec.node_name}")
    payload = {
        "action": action,
        "pod_uid": pod.metadata.uid,
        "user_id": employee,
        "volume_name": VOLUME_NAME,
        "sub_dir": SUB_DIR,
    }
    url = f"http://{daemons[0].status.pod_ip}:9090"
    headers = {"Authorization": f"Bearer {DAEMON_TOKEN}"}
    last = None
    for _ in range(60):
        r = requests.post(url, json=payload, headers=headers, timeout=10)
        if r.ok:
            return
        last = r.text
        time.sleep(0.5)
    raise RuntimeError(f"storage daemon {action} failed: {last}")


def daemon_delete_workspace(employee: str):
    """Delete the employee's workspace (any daemon can, storage is shared)."""
    daemons = core.list_namespaced_pod(
        NAMESPACE, label_selector="app=storage-node-daemon").items
    ready = [d for d in daemons if d.status.pod_ip]
    if not ready:
        raise RuntimeError("no storage daemon available")
    r = requests.post(
        f"http://{ready[0].status.pod_ip}:9090",
        json={"action": "delete", "pod_uid": "unused", "user_id": employee,
              "volume_name": VOLUME_NAME, "sub_dir": SUB_DIR},
        headers={"Authorization": f"Bearer {DAEMON_TOKEN}"}, timeout=30)
    r.raise_for_status()


def openclaw_url(sandbox) -> str | None:
    fqdn = (sandbox.get("status") or {}).get("serviceFQDN")
    return f"http://{fqdn}:{OPENCLAW_PORT}/" if fqdn else None


def wait_app_ready(sandbox, deadline: float) -> bool:
    """Poll OpenClaw itself — pod Ready only means the spin-wait is running.

    Any HTTP response (even 4xx) proves the gateway process is up.
    """
    url = openclaw_url(sandbox)
    if url is None:
        return False
    while time.time() < deadline:
        try:
            requests.get(url, timeout=2)
            return True
        except requests.RequestException:
            time.sleep(0.2)
    return False


def upsert_alias(employee: str, sandbox_name: str):
    """Stable per-employee address: oc-<employee> -> sandbox headless svc.

    The sandbox keeps its pool-generated name after warm adoption, so the
    fixed employee-facing name is a CNAME the router resolves via DNS:
      /router/<ns>/oc-<employee>/<port>/... just works, and the alias is
    simply repointed on rebuild while suspend/resume needs no change at all.
    """
    body = client.V1Service(
        metadata=client.V1ObjectMeta(
            name=claim_name(employee), labels={"app": "openclaw-alias"}),
        spec=client.V1ServiceSpec(
            type="ExternalName",
            external_name=f"{sandbox_name}.{NAMESPACE}.svc.cluster.local"))
    try:
        core.create_namespaced_service(NAMESPACE, body)
    except client.ApiException as e:
        if e.status != 409:
            raise
        core.patch_namespaced_service(claim_name(employee), NAMESPACE, body)


def provision(employee: str, annotations: dict) -> tuple[dict, dict]:
    """Create claim -> adopt -> bind storage -> app ready. Returns
    (claim, timings). The timing breakdown is the PoC's primary startup
    measurement (adoption is the sub-second part; app_ready adds OpenClaw's
    own boot on top)."""
    t0 = time.monotonic()
    crd.create_namespaced_custom_object(
        GROUP, VERSION, NAMESPACE, "sandboxclaims", {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "SandboxClaim",
            "metadata": {"name": claim_name(employee),
                         "annotations": annotations},
            "spec": {
                "warmPoolRef": {"name": POOL},
                # The ONLY warm-compatible per-claim customization: labels
                # under the sandbox.users.io domain, stamped onto the
                # adopted pod so the portal can find it.
                "additionalPodMetadata": {
                    "labels": {EMPLOYEE_LABEL: employee}},
            },
        })
    deadline = time.time() + WAKE_TIMEOUT
    claim = sandbox = None
    while time.time() < deadline:
        claim = get_claim(employee)
        sandbox = sandbox_of(claim) if claim else None
        if sandbox is not None:
            break
        time.sleep(0.05)  # tight poll: adoption is the sub-second event
    if sandbox is None:
        raise RuntimeError("warm adoption timed out")
    t_adopted = time.monotonic()

    pod = None
    while time.time() < deadline and pod is None:
        pod = pod_of(employee)
        time.sleep(0.05) if pod is None else None
    if pod is None:
        raise RuntimeError("adopted pod never appeared")
    daemon_bind(pod, employee)
    t_bound = time.monotonic()

    if not wait_app_ready(sandbox, deadline):
        raise RuntimeError("OpenClaw did not come up")
    t_app = time.monotonic()

    upsert_alias(employee, sandbox["metadata"]["name"])
    timings = {
        "adopted_ms": round((t_adopted - t0) * 1000),
        "bound_ms": round((t_bound - t_adopted) * 1000),
        "app_ready_ms": round((t_app - t_bound) * 1000),
        "total_ms": round((t_app - t0) * 1000),
    }
    return claim, timings


def authorized(claim) -> bool:
    token = request.headers.get("Authorization", "").removeprefix("Bearer ")
    if not token:
        return False
    got = hashlib.sha256(token.encode()).hexdigest()
    if ADMIN_TOKEN_SHA256 and hmac.compare_digest(got, ADMIN_TOKEN_SHA256):
        return True  # fleet admin (rolling updates, bulk ops)
    want = (claim["metadata"].get("annotations") or {}).get(TOKEN_ANNOTATION, "")
    return secrets.compare_digest(got, want)


@app.get("/healthz")
def healthz():
    return "ok"


@app.post("/employees")
def create_employee():
    employee = (request.get_json(silent=True) or {}).get("employee", "")
    if not isinstance(employee, str) or not DNS1123_LABEL.fullmatch(employee) \
            or len(employee) > MAX_EMPLOYEE_LEN:
        return jsonify(error="body must be {'employee': '<dns-1123 label>'}"), 400
    if get_claim(employee) is not None:
        return jsonify(error="employee exists"), 409
    token = secrets.token_urlsafe(32)
    try:
        claim, timings = provision(employee, {
            TOKEN_ANNOTATION: hashlib.sha256(token.encode()).hexdigest()})
    except client.ApiException as e:
        if e.status == 409:  # lost a concurrent-signup race
            return jsonify(error="employee exists"), 409
        raise
    last_activity[employee] = time.time()
    return jsonify(
        employee=employee,
        token=token,  # shown once; only the hash is stored
        sandbox=(claim.get("status") or {}).get("sandbox", {}).get("name"),
        path=f"/router/{NAMESPACE}/{claim_name(employee)}/{OPENCLAW_PORT}/",
        timings=timings,
    ), 201


@app.get("/employees/<employee>")
def get_employee(employee):
    claim = get_claim(employee)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    sandbox = sandbox_of(claim)
    return jsonify(employee=employee, state=derive_state(sandbox),
                   sandbox=(sandbox or {}).get("metadata", {}).get("name"),
                   path=f"/router/{NAMESPACE}/{claim_name(employee)}/{OPENCLAW_PORT}/")


@app.post("/employees/<employee>/suspend")
def suspend_employee(employee):
    claim = get_claim(employee)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    sandbox = sandbox_of(claim)
    if sandbox is None:
        return jsonify(error="no sandbox"), 409
    set_operating_mode(sandbox["metadata"]["name"], "Suspended")
    return jsonify(employee=employee, state="Suspending",
                   note="pod is torn down; Service, alias and workspace survive")


@app.post("/employees/<employee>/wake")
def wake_employee(employee):
    claim = get_claim(employee)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    sandbox = sandbox_of(claim)
    if sandbox is None:
        return jsonify(error="no sandbox"), 409
    t0 = time.monotonic()
    wall0 = time.time()
    set_operating_mode(sandbox["metadata"]["name"], "Running")
    # The resumed pod is a NEW pod: it spin-waits again, so the workspace
    # must be re-bound before OpenClaw restarts from persisted state.
    deadline = time.time() + WAKE_TIMEOUT
    pod = None
    while time.time() < deadline and pod is None:
        pod = pod_of(employee, not_before=wall0 - 1)
        time.sleep(0.1) if pod is None else None
    if pod is None:
        return jsonify(error="resumed pod never appeared"), 504
    daemon_bind(pod, employee)
    if not wait_app_ready(sandbox, deadline):
        return jsonify(error="OpenClaw did not come back", ), 504
    last_activity[employee] = time.time()
    return jsonify(employee=employee, state="Ready",
                   wake_ms=round((time.monotonic() - t0) * 1000))


@app.post("/employees/<employee>/rebuild")
def rebuild_employee(employee):
    """Rolling-update primitive: swap the employee onto a fresh warm spare
    (which carries the CURRENT template image/resources). Downtime is the
    reported downtime_ms; identity (alias, token) and workspace persist."""
    claim = get_claim(employee)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    annotations = {TOKEN_ANNOTATION:
                   (claim["metadata"].get("annotations") or {})
                   .get(TOKEN_ANNOTATION, "")}
    t0 = time.monotonic()
    crd.delete_namespaced_custom_object(
        GROUP, VERSION, NAMESPACE, "sandboxclaims", claim_name(employee))
    deadline = time.time() + WAKE_TIMEOUT
    while time.time() < deadline and get_claim(employee) is not None:
        time.sleep(0.1)
    if get_claim(employee) is not None:
        return jsonify(error="old claim did not go away"), 504
    _, timings = provision(employee, annotations)
    last_activity[employee] = time.time()
    return jsonify(employee=employee, timings=timings,
                   downtime_ms=round((time.monotonic() - t0) * 1000))


@app.delete("/employees/<employee>")
def delete_employee(employee):
    claim = get_claim(employee)
    if claim is None:
        return jsonify(error="not found"), 404
    if not authorized(claim):
        return jsonify(error="unauthorized"), 401
    purge = request.args.get("purge", "").lower() == "true"
    # Best-effort unbind so the host mount does not linger while the pod
    # terminates (the kubelet cleans the emptyDir afterwards).
    pod = pod_of(employee)
    if pod is not None:
        try:
            daemon_bind(pod, employee, action="unbind")
        except Exception as e:  # noqa: BLE001 - cleanup is best-effort
            print(f"unbind {employee}: {e}", flush=True)
    try:
        crd.delete_namespaced_custom_object(
            GROUP, VERSION, NAMESPACE, "sandboxclaims", claim_name(employee))
    except client.ApiException as e:
        if e.status != 404:
            raise
    try:
        core.delete_namespaced_service(claim_name(employee), NAMESPACE)
    except client.ApiException as e:
        if e.status != 404:
            raise
    if purge:
        daemon_delete_workspace(employee)
    last_activity.pop(employee, None)
    return jsonify(employee=employee, purged=purge,
                   note="claim deleted; sandbox, pod and Service cascade")


def idle_sweeper():
    """Optional: suspend sandboxes idle for IDLE_TIMEOUT seconds (the cost
    dial for 8h/12h/24h runtime profiles, test-checklist item 7). Off by
    default; portal-mediated traffic is the only activity signal here, so
    only enable it when clients touch the portal (not just the router)."""
    while True:
        time.sleep(30)
        try:
            claims = crd.list_namespaced_custom_object(
                GROUP, VERSION, NAMESPACE, "sandboxclaims")["items"]
            for c in claims:
                employee = c["metadata"]["name"].removeprefix("oc-")
                sandbox = sandbox_of(c)
                if sandbox is None or derive_state(sandbox) != "Ready":
                    continue
                idle = time.time() - last_activity.setdefault(
                    employee, time.time())
                if idle >= IDLE_TIMEOUT:
                    print(f"suspending {employee} (idle {int(idle)}s)",
                          flush=True)
                    set_operating_mode(sandbox["metadata"]["name"],
                                       "Suspended")
        except Exception as e:  # noqa: BLE001 - keep the daemon thread alive
            print(f"sweeper: transient error, retrying: {e}", flush=True)


if __name__ == "__main__":
    if IDLE_TIMEOUT > 0:
        threading.Thread(target=idle_sweeper, daemon=True).start()
    app.run(host="0.0.0.0", port=8080, threaded=True)
