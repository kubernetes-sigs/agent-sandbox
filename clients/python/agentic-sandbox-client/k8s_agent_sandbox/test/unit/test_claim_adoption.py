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

from k8s_agent_sandbox.claim_adoption import validate_claim_name


class TestClaimNameValidation(unittest.TestCase):

    def test_accepts_kubernetes_dns_subdomain_length_boundaries(self):
        for claim_name in ("a" * 64, "a" * 253):
            with self.subTest(length=len(claim_name)):
                validate_claim_name(claim_name)

    def test_rejects_names_beyond_kubernetes_dns_subdomain_limit(self):
        with self.assertRaisesRegex(ValueError, "253 characters"):
            validate_claim_name("a" * 254)

    def test_rejects_invalid_dns_subdomain_characters(self):
        for claim_name in ("UPPERCASE", "has_underscore", "-leading", "trailing-"):
            with self.subTest(claim_name=claim_name):
                with self.assertRaisesRegex(ValueError, "DNS-1123"):
                    validate_claim_name(claim_name)
