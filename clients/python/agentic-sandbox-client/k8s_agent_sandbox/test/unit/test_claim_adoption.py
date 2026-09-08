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

"""Unit tests for deterministic SandboxClaim adoption helpers."""

import unittest

from pydantic import BaseModel, ValidationError

from k8s_agent_sandbox.claim_adoption import (
    ValidatedClaimIdentity,
    get_ready_sandbox_name,
    validate_claim_for_adoption,
    validate_claim_name,
)
from k8s_agent_sandbox.claim_ownership import (
    ClaimLookupOperation,
    ClaimOwnership,
)
from k8s_agent_sandbox.test.unit.claim_adoption_test_support import (
    CLAIM_NAME,
    NAMESPACE,
    POD_ANNOTATIONS,
    POD_LABELS,
    REQUESTED_LABELS,
    VOLUME_CLAIM_TEMPLATES,
    WARMPOOL,
    matching_claim,
)


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


class TestClaimStatus(unittest.TestCase):

    def test_ready_condition_does_not_treat_warm_pool_reason_as_failure(self):
        claim = matching_claim()
        claim["status"] = {
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "reason": "WarmPoolNotFound",
                    "observedGeneration": 1,
                }
            ],
            "sandbox": {"name": "ready-sandbox"},
        }

        self.assertEqual(
            get_ready_sandbox_name(claim, CLAIM_NAME), "ready-sandbox"
        )

    def test_ready_condition_without_observed_generation_is_supported(self):
        claim = matching_claim()
        claim["status"] = {
            "conditions": [{"type": "Ready", "status": "True"}],
            "sandbox": {"name": "ready-sandbox"},
        }

        self.assertEqual(
            get_ready_sandbox_name(claim, CLAIM_NAME), "ready-sandbox"
        )


class TestClaimValidation(unittest.TestCase):

    def test_server_defaulted_spec_fields_are_ignored(self):
        claim = matching_claim()
        claim["spec"]["futureBehavior"] = {"enabled": True}
        claim["spec"]["otherBehavior"] = True

        identity = validate_claim_for_adoption(
            claim,
            claim_name=CLAIM_NAME,
            namespace=NAMESPACE,
            warmpool=WARMPOOL,
            labels=REQUESTED_LABELS,
            lifecycle=None,
            volume_claim_templates=VOLUME_CLAIM_TEMPLATES,
            pod_metadata={
                "labels": POD_LABELS,
                "annotations": POD_ANNOTATIONS,
            },
            env=None,
        )

        self.assertEqual(identity.uid, "claim-uid")


class TestClaimModels(unittest.TestCase):

    def test_validated_identity_is_a_frozen_pydantic_model(self):
        identity = ValidatedClaimIdentity(
            resource_version="resource-version", uid="claim-uid"
        )

        self.assertIsInstance(identity, BaseModel)
        with self.assertRaises(ValidationError):
            identity.uid = "replacement-uid"

    def test_lookup_state_uses_a_pydantic_model(self):
        lookup = ClaimLookupOperation()

        self.assertIsInstance(lookup, BaseModel)

    def test_lookup_operations_keep_identity_equality(self):
        first = ClaimLookupOperation()
        second = ClaimLookupOperation()

        self.assertNotEqual(first, second)


class TestClaimOwnership(unittest.TestCase):

    def test_automatic_ownership_requires_non_empty_claim_uid(self):
        ownership = ClaimOwnership()
        key = ("test-namespace", "generated-claim")

        for missing_uid in (None, ""):
            with self.subTest(claim_uid=missing_uid):
                ownership.register_automatic(key, missing_uid)

                self.assertNotIn(key, ownership.automatic_cleanup_claims)
                self.assertNotIn(key, ownership.automatic_cleanup_claim_uids)

    def test_failed_generated_claim_without_uid_is_not_deleted(self):
        ownership = ClaimOwnership()
        key = ("test-namespace", "generated-claim")

        should_delete = ownership.failed_generated_needs_delete(
            key,
            has_registered_handle=False,
            claim_uid=None,
        )

        self.assertFalse(should_delete)

    def test_caller_owned_name_rejects_automatic_ownership(self):
        ownership = ClaimOwnership()
        key = ("test-namespace", "generated-claim")

        ownership.mark_caller_owned(key)
        ownership.register_automatic(key, "generated-uid")
        should_delete = ownership.failed_generated_needs_delete(
            key,
            has_registered_handle=False,
            claim_uid="generated-uid",
        )

        self.assertFalse(should_delete)
        self.assertIn(key, ownership.caller_owned_claims)
        self.assertNotIn(key, ownership.automatic_cleanup_claims)

    def test_discard_allows_a_new_automatic_ownership_epoch(self):
        ownership = ClaimOwnership()
        key = ("test-namespace", "test-claim")

        ownership.mark_caller_owned(key)
        ownership.discard(key)
        ownership.register_automatic(key, "replacement-uid")

        self.assertIn(key, ownership.automatic_cleanup_claims)
        self.assertEqual(
            ownership.automatic_cleanup_uid(key), "replacement-uid"
        )
