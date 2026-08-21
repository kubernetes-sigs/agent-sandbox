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

"""Build watched-namespace RBAC instructions from generated ClusterRoles."""

WATCHED_NAMESPACE = "watched-namespace"
CONTROLLER_NAMESPACE = "controller-namespace"
CLUSTER_SCOPED = "cluster-scoped"


# controller-gen does not record whether a resource is namespaced. Keep an
# explicit classification so generation fails when a new permission has not
# been reviewed for the least-privilege namespaced deployment model.
RESOURCE_SCOPES = {
    ("", "events"): WATCHED_NAMESPACE,
    ("", "persistentvolumeclaims"): WATCHED_NAMESPACE,
    ("", "pods"): WATCHED_NAMESPACE,
    ("", "services"): WATCHED_NAMESPACE,
    ("agents.x-k8s.io", "sandboxes"): WATCHED_NAMESPACE,
    ("agents.x-k8s.io", "sandboxes/finalizers"): WATCHED_NAMESPACE,
    ("agents.x-k8s.io", "sandboxes/status"): WATCHED_NAMESPACE,
    ("events.k8s.io", "events"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxclaims"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxclaims/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxclaims/status"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxtemplates"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxtemplates/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools/status"): WATCHED_NAMESPACE,
    ("networking.k8s.io", "networkpolicies"): WATCHED_NAMESPACE,
    ("coordination.k8s.io", "leases"): CONTROLLER_NAMESPACE,
    ("apiextensions.k8s.io", "customresourcedefinitions"): CLUSTER_SCOPED,
}


def watched_namespace_rules(cluster_role):
    """Return rules safe to grant in each watched namespace."""
    if cluster_role.get("kind") != "ClusterRole":
        raise ValueError("expected a ClusterRole document")

    result = []
    for rule in cluster_role.get("rules", []):
        if rule.get("nonResourceURLs"):
            raise ValueError("non-resource URL permissions are not supported")

        api_groups = rule.get("apiGroups", [])
        resources = rule.get("resources", [])
        if not api_groups or not resources:
            raise ValueError("RBAC rules must contain apiGroups and resources")

        scopes = set()
        for api_group in api_groups:
            for resource in resources:
                key = (api_group, resource)
                try:
                    scopes.add(RESOURCE_SCOPES[key])
                except KeyError as err:
                    raise ValueError(
                        f"unclassified RBAC resource {api_group or 'core'}/{resource}"
                    ) from err

        if len(scopes) != 1:
            raise ValueError(
                "RBAC rule mixes resources with different namespace scopes: "
                f"apiGroups={api_groups}, resources={resources}"
            )
        if scopes == {WATCHED_NAMESPACE}:
            result.append(rule)

    return result
