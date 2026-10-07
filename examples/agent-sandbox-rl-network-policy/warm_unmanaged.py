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
"""Warm one image under the one-policy-per-namespace layout and report what the
cluster shows: the template's networkPolicyManagement, the NetworkPolicies in the
namespace and which of them a SandboxTemplate owns. Tears the pool down at the end.
Apply manifests/fleet-network-policy.yaml into NAMESPACE first (see README).
Env-configured:

  KUBE_CONTEXT=<ctx> NAMESPACE=agent-sandbox-rl IMAGE=python:3.12-slim \
  NODE_SELECTOR_KEY=cloud.google.com/gke-nodepool NODE_SELECTOR_VAL=<pool> \
  RUNTIME_CLASS=gvisor python warm_unmanaged.py

Before it warms anything it checks that some NetworkPolicy's podSelector covers the
labels the fleet's pods carry, and exits 1 without creating a pod if none does: an
Unmanaged sandbox with no policy is not isolated, and a policy that exists but
selects other pods does not help. After the warm it exits 1 if the template is not
Unmanaged or a template-owned policy exists for it.
"""
import json
import logging
import os
import sys

from kubernetes import client

from agent_sandbox_rl import ClusterConfig, FleetConfig, SandboxFleet, TemplateSpec, constants
from agent_sandbox_rl.preflight import pod_selector_covers
from agent_sandbox_rl.sources import ListSource, Task

logging.basicConfig(level=logging.INFO,
                    format="%(asctime)s %(levelname)s %(name)s %(message)s")



def _env(name, default):
  return os.getenv(name, default)


def covering_policies(fleet: SandboxFleet, namespace: str) -> list[str]:
  """Names of the NetworkPolicies in ``namespace`` whose podSelector selects the
  labels every fleet pod carries. Read through the fleet's own cluster client, so
  it checks the cluster the fleet warms on."""
  cluster = next(iter(fleet.registry))
  labels = cluster.resources.fleet_pod_labels()
  policies = client.NetworkingV1Api(cluster.api_client).list_namespaced_network_policy(
      namespace).items
  return [p.metadata.name for p in policies
          if pod_selector_covers(p.spec.pod_selector, labels)]


def report(fleet: SandboxFleet, namespace: str) -> dict:
  """What the API server shows for this run's templates and the namespace's
  policies. Uses the fleet's own cluster client so it sees the same cluster."""
  cluster = next(iter(fleet.registry))
  templates = cluster.custom_api.list_namespaced_custom_object(
      group=constants.GROUP, version=constants.VERSION, namespace=namespace,
      plural=constants.TEMPLATES_PLURAL,
      label_selector=f"{constants.RUN_ID_LABEL}={fleet.run_id}")["items"]
  policies = client.NetworkingV1Api(cluster.api_client).list_namespaced_network_policy(
      namespace).items
  mine = {t["metadata"]["name"] for t in templates}
  template_owned = [
      p.metadata.name for p in policies
      if any(o.kind == "SandboxTemplate" for o in (p.metadata.owner_references or []))]
  return {
      "run_id": fleet.run_id,
      "templates": [{"name": t["metadata"]["name"],
                     "networkPolicyManagement":
                         (t.get("spec") or {}).get("networkPolicyManagement", "Managed")}
                    for t in templates],
      "network_policies_in_namespace": len(policies),
      "template_owned_policies": template_owned,
      "template_owned_policies_for_this_run": [
          n for n in template_owned if n.removesuffix("-network-policy") in mine],
      "policies_covering_fleet_pods": covering_policies(fleet, namespace),
  }


def main() -> int:
  namespace = _env("NAMESPACE", "agent-sandbox-rl")
  context = _env("KUBE_CONTEXT", "") or None
  node_selector = None
  if _env("NODE_SELECTOR_KEY", "") and _env("NODE_SELECTOR_VAL", ""):
    node_selector = {os.environ["NODE_SELECTOR_KEY"]: os.environ["NODE_SELECTOR_VAL"]}

  fleet = SandboxFleet(FleetConfig(
      clusters=[ClusterConfig(name=context or "default", context=context,
                              namespace=namespace)],
      max_concurrent=1, max_warmpool_size=1,
      ready_timeout=int(_env("SANDBOX_READY_TIMEOUT", "600")),
      template=TemplateSpec(
          network_policy_management="Unmanaged",
          runtime_class=_env("RUNTIME_CLASS", "") or None,
          node_selector=node_selector)))
  fleet.load_tasks(ListSource([Task(id="network-policy-smoke",
                                    image=_env("IMAGE", "python:3.12-slim"))]))
  if not covering_policies(fleet, namespace):
    print(f"FAIL no NetworkPolicy in {namespace} selects the fleet's pods; apply "
          "manifests/fleet-network-policy.yaml before warming Unmanaged templates",
          file=sys.stderr)
    return 1
  try:
    fleet.setup()  # preflight -> plan -> warm the one pool and wait for Ready
    result = report(fleet, namespace)
  finally:
    fleet.teardown()

  print(json.dumps(result, indent=2))
  problems = []
  if not result["templates"]:
    problems.append("no template labelled with this run id was found")
  for t in result["templates"]:
    if t["networkPolicyManagement"] != "Unmanaged":
      problems.append(f"template {t['name']} is {t['networkPolicyManagement']}")
  if result["template_owned_policies_for_this_run"]:
    problems.append("the controller still owns a policy for this run's template: "
                    + ", ".join(result["template_owned_policies_for_this_run"]))
  if not result["policies_covering_fleet_pods"]:
    problems.append(f"no NetworkPolicy in {namespace} selects the fleet's pods; "
                    "apply manifests/fleet-network-policy.yaml")
  for p in problems:
    print("FAIL", p, file=sys.stderr)
  if not problems:
    print("PASS one namespace-wide policy, no per-template policy", file=sys.stderr)
  return 1 if problems else 0


if __name__ == "__main__":
  sys.exit(main())
