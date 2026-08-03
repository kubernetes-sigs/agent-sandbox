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

import unittest

from k8s_agent_sandbox.utils import extract_sandbox_name_hash


class TestExtractSandboxNameHash(unittest.TestCase):
    def test_extract_sandbox_name_hash_empty(self):
        sandbox_object = {
            "status": {
                "selector": ""
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)

    def test_extract_sandbox_name_hash_single(self):
        sandbox_object = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash=abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_multi(self):
        sandbox_object = {
            "status": {
                "selector": "app=example-sandbox,agents.x-k8s.io/sandbox-name-hash=abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_extra_whitespace(self):
        sandbox_object = {
            "status": {
                "selector": "agents.x-k8s.io/sandbox-name-hash = abc12345"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertEqual(sandbox_name_hash, "abc12345")

    def test_extract_sandbox_name_hash_not_included(self):
        sandbox_object = {
            "status": {
                "selector": "app=example-sandbox"
            }
        }
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)

    def test_extract_sandbox_name_hash_no_status(self):
        sandbox_object = {}
        sandbox_name_hash = extract_sandbox_name_hash(sandbox_object)
        self.assertIsNone(sandbox_name_hash)


if __name__ == "__main__":
    unittest.main()
