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

"""RBAC scope classification for namespace-scoped controller deployments.

Every (apiGroup, resource) pair that appears in the controller's generated
ClusterRole must be classified here.  When ``go generate`` adds a new
+kubebuilder:rbac marker whose resource is not in RESOURCE_SCOPES, the
generate-namespaced-rbac script will fail loudly, forcing a deliberate review
of whether the new permission belongs in watched namespaces, the controller
namespace, or is cluster-scoped.
"""

import yaml

WATCHED_NAMESPACE = "WATCHED_NAMESPACE"
CONTROLLER_NAMESPACE = "CONTROLLER_NAMESPACE"

# Resources whose rules belong in the leader-election Role (leases for the LE
# lease itself; events for the LE library's event recording).
_LE_RESOURCES = frozenset({"leases", "events"})

# Maps (apiGroup, resource) to the namespace scope the permission belongs to.
# Keep sorted by apiGroup then resource for readability.
RESOURCE_SCOPES = {
    # Core API
    ("", "events"): WATCHED_NAMESPACE,
    ("", "persistentvolumeclaims"): WATCHED_NAMESPACE,
    ("", "pods"): WATCHED_NAMESPACE,
    ("", "services"): WATCHED_NAMESPACE,
    # Sandbox CRD
    ("agents.x-k8s.io", "sandboxes"): WATCHED_NAMESPACE,
    ("agents.x-k8s.io", "sandboxes/finalizers"): WATCHED_NAMESPACE,
    ("agents.x-k8s.io", "sandboxes/status"): WATCHED_NAMESPACE,
    # Leader election
    ("coordination.k8s.io", "leases"): CONTROLLER_NAMESPACE,
    # Events (dedicated group)
    ("events.k8s.io", "events"): WATCHED_NAMESPACE,
    # Extensions CRDs
    ("extensions.agents.x-k8s.io", "sandboxclaims"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxclaims/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxclaims/status"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxtemplates"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxtemplates/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools/finalizers"): WATCHED_NAMESPACE,
    ("extensions.agents.x-k8s.io", "sandboxwarmpools/status"): WATCHED_NAMESPACE,
    # Network policies
    ("networking.k8s.io", "networkpolicies"): WATCHED_NAMESPACE,
}


def _classify_rule(rule):
    """Return the set of scopes referenced by a single RBAC rule."""
    scopes = set()
    for group in rule.get("apiGroups", []):
        for resource in rule.get("resources", []):
            key = (group, resource)
            if key not in RESOURCE_SCOPES:
                raise ValueError(
                    f"unclassified RBAC resource {key!r}; add it to "
                    f"RESOURCE_SCOPES in dev/tools/shared/rbac_scopes.py"
                )
            scopes.add(RESOURCE_SCOPES[key])
    return scopes


def watched_namespace_rules(cluster_role_yaml):
    """Extract the WATCHED_NAMESPACE rules from a generated ClusterRole YAML.

    Returns only the rules list as a YAML string (no ClusterRole wrapper).
    Raises ValueError if any (apiGroup, resource) pair is not in RESOURCE_SCOPES
    or if the input is not a ClusterRole.
    """
    doc = yaml.safe_load(cluster_role_yaml)
    if doc is None or doc.get("kind") != "ClusterRole":
        raise ValueError("input must be a ClusterRole YAML document")
    rules = doc.get("rules") or []
    kept = []
    for rule in rules:
        scopes = _classify_rule(rule)
        if scopes == {WATCHED_NAMESPACE}:
            kept.append(rule)
        elif WATCHED_NAMESPACE in scopes:
            # Mixed-scope rule: split by extracting only the watched resources.
            watched_groups = {}
            for group in rule.get("apiGroups", []):
                watched_resources = [
                    r for r in rule.get("resources", [])
                    if RESOURCE_SCOPES.get((group, r)) == WATCHED_NAMESPACE
                ]
                if watched_resources:
                    watched_groups.setdefault(group, set()).update(watched_resources)
            if watched_groups:
                from collections import defaultdict
                by_resources = defaultdict(list)
                for g, rs in watched_groups.items():
                    by_resources[tuple(sorted(rs))].append(g)
                for rs, gs in by_resources.items():
                    kept.append({
                        "apiGroups": sorted(gs),
                        "resources": sorted(rs),
                        "verbs": rule["verbs"],
                    })
    return yaml.dump(kept, default_flow_style=False, sort_keys=False)


def leader_election_rules(cluster_role_yaml):
    """Extract the leader-election rules from a generated ClusterRole YAML.

    Returns rules that reference any resource in ``_LE_RESOURCES`` (leases,
    events) as a YAML string.  The verbs are taken verbatim from the
    ClusterRole so they track controller-gen output.

    Raises ValueError for unclassified resources or non-ClusterRole input.
    """
    doc = yaml.safe_load(cluster_role_yaml)
    if doc is None or doc.get("kind") != "ClusterRole":
        raise ValueError("input must be a ClusterRole YAML document")
    rules = doc.get("rules") or []
    kept = []
    for rule in rules:
        _classify_rule(rule)  # validate all resources are classified
        resources = set(rule.get("resources", []))
        if resources & _LE_RESOURCES:
            kept.append(rule)
    return yaml.dump(kept, default_flow_style=False, sort_keys=False)
