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
"""Ownership bookkeeping for SandboxClaim handles."""

from pydantic import BaseModel


ClaimKey = tuple[str, str]


class ClaimLookupOperation(BaseModel):
    """Identity token invalidated when deliberate deletion wins a race."""

    invalidated: bool = False

    def __eq__(self, other: object) -> bool:
        return self is other

    __hash__ = object.__hash__


class ClaimOwnership:
    """Track automatic and caller-owned Claims.

    Every method must be called while the owning client's registry lock is held.

    An explicitly supplied Claim name is caller-owned from the start of the
    operation, whether creation succeeds or fails. Only generated or reattached
    Claims with an observed UID are eligible for automatic cleanup.
    """

    def __init__(self) -> None:
        self.automatic_cleanup_claims: set[ClaimKey] = set()
        self.automatic_cleanup_claim_uids: dict[ClaimKey, str] = {}
        self.caller_owned_claims: set[ClaimKey] = set()
        self._lookup_operations: dict[ClaimKey, list[ClaimLookupOperation]] = {}

    def begin_lookup(self, key: ClaimKey) -> ClaimLookupOperation:
        """Capture an identity token for an in-flight Claim lookup."""
        operation = ClaimLookupOperation()
        self._lookup_operations.setdefault(key, []).append(operation)
        return operation

    def lookup_is_valid(
        self, key: ClaimKey, operation: ClaimLookupOperation
    ) -> bool:
        """Return whether deletion has not superseded a lookup."""
        operations = self._lookup_operations.get(key, [])
        return operation in operations and not operation.invalidated

    def finish_lookup(
        self, key: ClaimKey, operation: ClaimLookupOperation
    ) -> None:
        """Release an in-flight lookup token."""
        operations = self._lookup_operations.get(key)
        if operations is None or operation not in operations:
            raise RuntimeError("Claim lookup operation changed unexpectedly.")
        operations.remove(operation)
        if not operations:
            self._lookup_operations.pop(key)

    def mark_caller_owned(self, key: ClaimKey) -> None:
        """Make an explicitly named Claim ineligible for automatic cleanup."""
        self.caller_owned_claims.add(key)
        self.automatic_cleanup_claims.discard(key)
        self.automatic_cleanup_claim_uids.pop(key, None)

    def register_automatic(self, key: ClaimKey, claim_uid: str | None) -> None:
        """Record automatic ownership unless an explicit call owns the name."""
        if not claim_uid:
            return
        if key in self.caller_owned_claims:
            return
        self.automatic_cleanup_claims.add(key)
        self.automatic_cleanup_claim_uids[key] = claim_uid

    def failed_generated_needs_delete(
        self,
        key: ClaimKey,
        *,
        has_registered_handle: bool,
        claim_uid: str | None,
    ) -> bool:
        """Return whether a failed generated Claim can be deleted safely."""
        return (
            not has_registered_handle
            and bool(claim_uid)
            and key not in self.caller_owned_claims
        )

    def automatic_cleanup_uid(self, key: ClaimKey) -> str | None:
        """Return the UID that constrains automatic deletion for a Claim."""
        return self.automatic_cleanup_claim_uids.get(key)

    def should_retire_handle(self, key: ClaimKey) -> bool:
        """Return whether a detached handle could delete an automatic Claim."""
        return (
            key in self.automatic_cleanup_claims
            and key not in self.caller_owned_claims
        )

    def can_delete_automatic_claim(self, key: ClaimKey) -> bool:
        """Return whether this client still owns automatic cleanup."""
        return (
            key in self.automatic_cleanup_claims
            and key not in self.caller_owned_claims
        )

    def take_automatic_cleanup(
        self, key: ClaimKey
    ) -> tuple[bool, str | None]:
        """Reserve one automatic cleanup attempt and return its observed UID."""
        if not self.can_delete_automatic_claim(key):
            return False, None
        self.automatic_cleanup_claims.discard(key)
        return True, self.automatic_cleanup_claim_uids.pop(key, None)

    def discard_automatic_if_uid(
        self, key: ClaimKey, expected_uid: str
    ) -> None:
        """Forget automatic ownership only for the deleted Claim identity."""
        if self.automatic_cleanup_claim_uids.get(key) != expected_uid:
            return
        self.automatic_cleanup_claims.discard(key)
        self.automatic_cleanup_claim_uids.pop(key, None)

    def discard(self, key: ClaimKey) -> None:
        """Forget completed ownership after deliberate deletion."""
        self.automatic_cleanup_claims.discard(key)
        self.automatic_cleanup_claim_uids.pop(key, None)
        self.caller_owned_claims.discard(key)
        for operation in self._lookup_operations.get(key, []):
            operation.invalidated = True
