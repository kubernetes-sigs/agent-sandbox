#!/usr/bin/env python3

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

import os
import sys
import unittest

import yaml

# Make the test importable regardless of how it is invoked.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from rbac_scopes import RESOURCE_SCOPES, leader_election_rules, watched_namespace_rules

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


def _read_generated(name):
    path = os.path.join(REPO_ROOT, "helm", "files", name)
    with open(path) as f:
        return f.read()


class WatchedNamespaceRulesTest(unittest.TestCase):
    """Tests for the RBAC scope filtering logic."""

    def test_core_rbac_excludes_leases(self):
        raw = _read_generated("rbac.generated.yaml")
        result = yaml.safe_load(watched_namespace_rules(raw))
        resources = set()
        for rule in result:
            for r in rule.get("resources", []):
                resources.add(r)
        self.assertNotIn("leases", resources)
        self.assertIn("pods", resources)
        self.assertIn("sandboxes", resources)

    def test_extensions_rbac_excludes_leases(self):
        raw = _read_generated("extensions-rbac.generated.yaml")
        result = yaml.safe_load(watched_namespace_rules(raw))
        resources = set()
        for rule in result:
            for r in rule.get("resources", []):
                resources.add(r)
        self.assertNotIn("leases", resources)
        self.assertIn("sandboxclaims", resources)
        self.assertIn("networkpolicies", resources)

    def test_unrecognized_resource_raises(self):
        cr = yaml.dump({
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRole",
            "metadata": {"name": "test"},
            "rules": [{
                "apiGroups": ["unknown.group"],
                "resources": ["foos"],
                "verbs": ["get"],
            }],
        })
        with self.assertRaises(ValueError) as ctx:
            watched_namespace_rules(cr)
        self.assertIn("unclassified", str(ctx.exception))

    def test_non_clusterrole_raises(self):
        doc = yaml.dump({
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "Role",
            "metadata": {"name": "test"},
            "rules": [],
        })
        with self.assertRaises(ValueError):
            watched_namespace_rules(doc)

    def test_output_is_valid_yaml_list(self):
        raw = _read_generated("rbac.generated.yaml")
        result = yaml.safe_load(watched_namespace_rules(raw))
        self.assertIsInstance(result, list)
        for rule in result:
            self.assertIn("apiGroups", rule)
            self.assertIn("resources", rule)
            self.assertIn("verbs", rule)

    def test_all_generated_resources_in_scope_map(self):
        for name in ("rbac.generated.yaml", "extensions-rbac.generated.yaml"):
            with self.subTest(file=name):
                raw = _read_generated(name)
                doc = yaml.safe_load(raw)
                for rule in doc.get("rules", []):
                    for group in rule.get("apiGroups", []):
                        for resource in rule.get("resources", []):
                            key = (group, resource)
                            self.assertIn(
                                key, RESOURCE_SCOPES,
                                f"{key} from {name} not in RESOURCE_SCOPES",
                            )

    def test_empty_rules(self):
        cr = yaml.dump({
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRole",
            "metadata": {"name": "empty"},
            "rules": [],
        })
        result = yaml.safe_load(watched_namespace_rules(cr))
        self.assertEqual(result, [])


class LeaderElectionRulesTest(unittest.TestCase):
    """Tests for the leader-election rule extraction."""

    def test_core_le_includes_leases_and_events(self):
        raw = _read_generated("rbac.generated.yaml")
        result = yaml.safe_load(leader_election_rules(raw))
        resources = set()
        for rule in result:
            for r in rule.get("resources", []):
                resources.add(r)
        self.assertIn("leases", resources)
        self.assertIn("events", resources)
        self.assertNotIn("pods", resources)

    def test_extensions_le_has_delete_on_leases(self):
        raw = _read_generated("extensions-rbac.generated.yaml")
        result = yaml.safe_load(leader_election_rules(raw))
        for rule in result:
            if "leases" in rule.get("resources", []):
                self.assertIn("delete", rule["verbs"])
                return
        self.fail("no lease rule found in extensions LE output")

    def test_extensions_le_has_update_on_events(self):
        raw = _read_generated("extensions-rbac.generated.yaml")
        result = yaml.safe_load(leader_election_rules(raw))
        for rule in result:
            if "events" in rule.get("resources", []):
                self.assertIn("update", rule["verbs"])
                return
        self.fail("no events rule found in extensions LE output")


class ParityTest(unittest.TestCase):
    """Verify that namespaced + LE rules cover the full ClusterRole."""

    def _rule_tuples(self, rules):
        """Convert rules list to a set of (group, resource, verb) tuples."""
        tuples = set()
        for rule in rules:
            for group in rule.get("apiGroups", []):
                for resource in rule.get("resources", []):
                    for verb in rule.get("verbs", []):
                        tuples.add((group, resource, verb))
        return tuples

    def test_namespaced_plus_le_covers_clusterrole(self):
        for name in ("rbac.generated.yaml", "extensions-rbac.generated.yaml"):
            with self.subTest(file=name):
                raw = _read_generated(name)
                full = yaml.safe_load(raw)
                full_tuples = self._rule_tuples(full.get("rules", []))
                ns_tuples = self._rule_tuples(
                    yaml.safe_load(watched_namespace_rules(raw))
                )
                le_tuples = self._rule_tuples(
                    yaml.safe_load(leader_election_rules(raw))
                )
                covered = ns_tuples | le_tuples
                missing = full_tuples - covered
                self.assertEqual(
                    missing, set(),
                    f"permissions in {name} not covered by namespaced + LE "
                    f"rules: {missing}",
                )


if __name__ == "__main__":
    unittest.main()
