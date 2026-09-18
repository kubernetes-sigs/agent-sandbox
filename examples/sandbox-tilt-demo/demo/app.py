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

"""Small FastAPI service used by the Agent Sandbox Tilt demo.

``POST /create`` creates a ``Sandbox`` custom resource and waits for it to go
Ready; the agent-sandbox controller provisions the backing pod, which prints a
message and then stays up so the Sandbox holds Ready. ``POST /delete`` removes
it again. Nothing is created until a route is called, so the demo shows the
Sandbox being provisioned on request.

The app always runs inside the cluster, so it authenticates with its
ServiceAccount via in-cluster config.
"""

import logging
import os
import time

from fastapi import FastAPI
from kubernetes import client, config

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("agent-sandbox-demo-app")

API_GROUP = "agents.x-k8s.io"
API_VERSION = "v1beta1"
API_PLURAL = "sandboxes"

SANDBOX_NAME = os.environ.get("SANDBOX_NAME", "demo")
SANDBOX_NAMESPACE = os.environ.get("SANDBOX_NAMESPACE", "default")
SANDBOX_IMAGE = os.environ.get("SANDBOX_IMAGE", "alpine:latest")
SANDBOX_MESSAGE = os.environ.get("SANDBOX_MESSAGE", "Hello Sandbox!")
SANDBOX_DURATION = int(os.environ.get("SANDBOX_DURATION", "86400"))
READY_TIMEOUT = int(os.environ.get("READY_TIMEOUT", "60"))

config.load_incluster_config()
api = client.CustomObjectsApi()


def build_sandbox_body():
    """Build the Sandbox spec the controller will reconcile into a pod.

    The container prints the message and then sleeps rather than exiting: a
    Sandbox whose pod terminates reports Finished/PodSucceeded instead of Ready.
    """
    return {
        "apiVersion": f"{API_GROUP}/{API_VERSION}",
        "kind": "Sandbox",
        "metadata": {"name": SANDBOX_NAME, "namespace": SANDBOX_NAMESPACE},
        "spec": {
            "podTemplate": {
                "spec": {
                    "containers": [
                        {
                            "name": "my-container",
                            "image": SANDBOX_IMAGE,
                            "command": [
                                "/bin/sh", "-c",
                                f"echo '{SANDBOX_MESSAGE}' && sleep {SANDBOX_DURATION}",
                            ],
                        }
                    ],
                }
            }
        },
    }


def create_sandbox():
    """Create the Sandbox, treating "already exists" as success."""
    try:
        return api.create_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION,
            namespace=SANDBOX_NAMESPACE, plural=API_PLURAL,
            body=build_sandbox_body())
    except client.ApiException as e:
        if e.status == 409:
            log.info("Sandbox %s already exists", SANDBOX_NAME)
            return fetch_sandbox()
        raise


def fetch_sandbox():
    return api.get_namespaced_custom_object(
        group=API_GROUP, version=API_VERSION,
        namespace=SANDBOX_NAMESPACE, plural=API_PLURAL, name=SANDBOX_NAME)


def wait_for_ready(timeout=READY_TIMEOUT):
    """Poll until the Sandbox reports Ready=True or the timeout elapses."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        sandbox = fetch_sandbox()
        for condition in sandbox.get("status", {}).get("conditions", []):
            if condition.get("type") == "Ready" and condition.get("status") == "True":
                return sandbox
        time.sleep(2)
    return fetch_sandbox()


app = FastAPI()


@app.post("/create")
def create():
    """Create the Sandbox and wait for the controller to bring its pod up."""
    log.info("creating Sandbox %s/%s", SANDBOX_NAMESPACE, SANDBOX_NAME)
    create_sandbox()
    return {"created": SANDBOX_NAME, "namespace": SANDBOX_NAMESPACE,
            "status": wait_for_ready().get("status", {})}


@app.post("/delete")
def delete():
    """Delete the Sandbox; the controller tears the backing pod down.

    Deleting something already gone is treated as success, mirroring create's
    handling of "already exists": calling either route twice should not error.
    """
    log.info("deleting Sandbox %s/%s", SANDBOX_NAMESPACE, SANDBOX_NAME)
    try:
        api.delete_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION,
            namespace=SANDBOX_NAMESPACE, plural=API_PLURAL, name=SANDBOX_NAME)
    except client.ApiException as e:
        if e.status != 404:
            raise
        log.info("Sandbox %s does not exist", SANDBOX_NAME)
        return {"deleted": False, "name": SANDBOX_NAME,
                "namespace": SANDBOX_NAMESPACE}
    return {"deleted": True, "name": SANDBOX_NAME, "namespace": SANDBOX_NAMESPACE}
