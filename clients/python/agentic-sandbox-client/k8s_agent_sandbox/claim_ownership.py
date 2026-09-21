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


class ClaimOperation(BaseModel):
    """Creation or lookup token invalidated when deletion wins a race."""

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

    Deletion reserves the name until the Kubernetes call finishes. New creation
    or lookup must retry while reserved; earlier operations are invalidated. This
    keeps same-UID adoption from racing a DELETE outside the registry lock.
    """

    def __init__(self) -> None:
        self.automatic_cleanup_claims: set[ClaimKey] = set()
        self.automatic_cleanup_claim_uids: dict[ClaimKey, str] = {}
        self.caller_owned_claims: set[ClaimKey] = set()
        self._operations: dict[ClaimKey, list[ClaimOperation]] = {}
        self._deleting_claims: set[ClaimKey] = set()

    def ensure_not_deleting(self, key: ClaimKey) -> None:
        """Reject a new operation while deletion owns this Claim name."""
        if key in self._deleting_claims:
            namespace, claim_name = key
            raise RuntimeError(
                f"SandboxClaim '{claim_name}' in namespace '{namespace}' "
                "is being deleted concurrently; retry the operation."
            )

    def begin_deletion(self, key: ClaimKey) -> None:
        """Reserve a name across deletion I/O without holding the client lock."""
        self.ensure_not_deleting(key)
        self._deleting_claims.add(key)
        for operation in self._operations.get(key, []):
            operation.invalidated = True

    def finish_deletion(self, key: ClaimKey) -> None:
        """Release the reservation after success, failure, or cancellation."""
        self._deleting_claims.remove(key)

    def begin_operation(self, key: ClaimKey) -> ClaimOperation:
        """Capture an identity token for in-flight creation or lookup."""
        self.ensure_not_deleting(key)
        operation = ClaimOperation()
        self._operations.setdefault(key, []).append(operation)
        return operation

    def operation_is_valid(
        self, key: ClaimKey, operation: ClaimOperation
    ) -> bool:
        """Return whether deletion has not superseded this operation."""
        operations = self._operations.get(key, [])
        return operation in operations and not operation.invalidated

    def finish_operation(
        self, key: ClaimKey, operation: ClaimOperation
    ) -> None:
        """Release an in-flight creation or lookup token."""
        operations = self._operations.get(key)
        if operations is None or operation not in operations:
            raise RuntimeError("Claim operation changed unexpectedly.")
        operations.remove(operation)
        if not operations:
            self._operations.pop(key)

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
            and key not in self._deleting_claims
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
            and key not in self._deleting_claims
        )

    def take_automatic_cleanup(
        self, key: ClaimKey
    ) -> tuple[bool, str | None]:
        """Reserve one automatic cleanup attempt and return its observed UID."""
        if not self.can_delete_automatic_claim(key):
            return False, None
        self.begin_deletion(key)
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
        for operation in self._operations.get(key, []):
            operation.invalidated = True
