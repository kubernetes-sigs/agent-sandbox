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

"""Tests for generated namespaced RBAC instructions."""

import os
import sys
import unittest
from unittest import mock

# Make the test importable regardless of how pytest is invoked.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import rbac_notes


class WatchedNamespaceRulesTest(unittest.TestCase):
    def test_keeps_only_watched_namespace_rules(self):
        watched_rule = {
            "apiGroups": ["", "events.k8s.io"],
            "resources": ["events"],
            "verbs": ["create", "patch"],
        }
        cluster_role = {
            "kind": "ClusterRole",
            "rules": [
                watched_rule,
                {
                    "apiGroups": ["coordination.k8s.io"],
                    "resources": ["leases"],
                    "verbs": ["get"],
                },
                {
                    "apiGroups": ["apiextensions.k8s.io"],
                    "resources": ["customresourcedefinitions"],
                    "verbs": ["get"],
                },
            ],
        }

        self.assertEqual([watched_rule], rbac_notes.watched_namespace_rules(cluster_role))

    def test_rejects_unclassified_resource(self):
        cluster_role = {
            "kind": "ClusterRole",
            "rules": [{
                "apiGroups": ["apps"],
                "resources": ["deployments"],
                "verbs": ["get"],
            }],
        }

        with self.assertRaisesRegex(ValueError, "unclassified RBAC resource apps/deployments"):
            rbac_notes.watched_namespace_rules(cluster_role)

    def test_rejects_mixed_scope_rule(self):
        cluster_role = {
            "kind": "ClusterRole",
            "rules": [{
                "apiGroups": [""],
                "resources": ["pods", "nodes"],
                "verbs": ["get"],
            }],
        }

        with mock.patch.dict(
            rbac_notes.RESOURCE_SCOPES,
            {("", "nodes"): rbac_notes.CLUSTER_SCOPED},
        ):
            with self.assertRaisesRegex(ValueError, "mixes resources with different namespace scopes"):
                rbac_notes.watched_namespace_rules(cluster_role)

    def test_rejects_non_cluster_role(self):
        with self.assertRaisesRegex(ValueError, "expected a ClusterRole"):
            rbac_notes.watched_namespace_rules({"kind": "Role", "rules": []})


if __name__ == "__main__":
    unittest.main()
