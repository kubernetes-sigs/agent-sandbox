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
"""Ownership bookkeeping for deterministic SandboxClaim operations."""

from pydantic import BaseModel


ClaimKey = tuple[str, str]


class ExplicitClaimOperations(BaseModel):
    active: int
    caller_owned_before: bool
    automatic_cleanup_before: bool
    committed: bool = False
    generated_cleanup_pending: bool = False
    generated_cleanup_uid: str | None = None
    invalidated: bool = False


class ClaimLookupOperation(BaseModel):
    """Identity token invalidated when deliberate deletion wins a race."""

    invalidated: bool = False

    def __eq__(self, other: object) -> bool:
        return self is other

    __hash__ = object.__hash__


class ClaimOwnership:
    """Track automatic and caller-owned Claims across overlapping operations.

    Every method must be called while the owning client's registry lock is held.
    """

    def __init__(self) -> None:
        self.automatic_cleanup_claims: set[ClaimKey] = set()
        self.automatic_cleanup_claim_uids: dict[ClaimKey, str | None] = {}
        self.caller_owned_claims: set[ClaimKey] = set()
        self._explicit_operations: dict[ClaimKey, ExplicitClaimOperations] = {}
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

    def begin_explicit(self, key: ClaimKey) -> ExplicitClaimOperations:
        """Protect a Claim while an explicitly named operation is in flight."""
        state = self._explicit_operations.get(key)
        if state is None:
            state = ExplicitClaimOperations(
                active=0,
                caller_owned_before=key in self.caller_owned_claims,
                automatic_cleanup_before=key in self.automatic_cleanup_claims,
            )
            self._explicit_operations[key] = state
            self.caller_owned_claims.add(key)
            self.automatic_cleanup_claims.discard(key)
        state.active += 1
        return state

    def finish_explicit(
        self,
        key: ClaimKey,
        operation: ExplicitClaimOperations,
        *,
        committed: bool,
        has_registered_handle: bool,
    ) -> tuple[bool, str | None]:
        """Finish an explicit operation and report deferred deletion work."""
        state = self._explicit_operations.get(key)
        if state is not operation and operation.invalidated:
            return False, None
        if state is not operation:
            raise RuntimeError("Explicit Claim ownership operation changed unexpectedly.")
        state.committed = state.committed or committed
        state.active -= 1
        if state.active:
            return False, None

        self._explicit_operations.pop(key)
        if state.committed:
            self.caller_owned_claims.add(key)
            self.automatic_cleanup_claims.discard(key)
            self.automatic_cleanup_claim_uids.pop(key, None)
        else:
            self._restore_previous_ownership(key, state)

        should_delete = (
            state.generated_cleanup_pending
            and state.generated_cleanup_uid is not None
            and key not in self.caller_owned_claims
            and not has_registered_handle
        )
        return should_delete, state.generated_cleanup_uid

    def register_automatic(self, key: ClaimKey, claim_uid: str | None) -> None:
        """Record automatic ownership unless a completed explicit call owns it."""
        if key in self._explicit_operations or key not in self.caller_owned_claims:
            self.automatic_cleanup_claims.add(key)
            self.automatic_cleanup_claim_uids[key] = claim_uid

    def failed_generated_needs_delete(
        self,
        key: ClaimKey,
        *,
        has_registered_handle: bool,
        claim_uid: str | None,
    ) -> bool:
        """Return whether a failed generated Claim can be deleted immediately."""
        if has_registered_handle or claim_uid is None:
            return False
        state = self._explicit_operations.get(key)
        if state is not None:
            if not state.caller_owned_before:
                state.generated_cleanup_pending = True
                state.generated_cleanup_uid = claim_uid
            return False
        return key not in self.caller_owned_claims

    def explicit_is_valid(
        self, key: ClaimKey, operation: ExplicitClaimOperations
    ) -> bool:
        """Return whether deliberate deletion has not superseded an operation."""
        return self._explicit_operations.get(key) is operation and not operation.invalidated

    def automatic_cleanup_uid(self, key: ClaimKey) -> str | None:
        """Return the UID that constrains automatic deletion for a Claim."""
        return self.automatic_cleanup_claim_uids.get(key)

    def should_retire_handle(self, key: ClaimKey) -> bool:
        """Return whether a detached handle could delete an automatic Claim."""
        state = self._explicit_operations.get(key)
        if state is not None:
            return not state.caller_owned_before and (
                state.automatic_cleanup_before
                or key in self.automatic_cleanup_claims
            )
        return (
            key in self.automatic_cleanup_claims
            and key not in self.caller_owned_claims
        )

    def can_delete_automatic_claim(self, key: ClaimKey) -> bool:
        """Return whether no explicit operation protects an automatic Claim."""
        return (
            key in self.automatic_cleanup_claims
            and key not in self.caller_owned_claims
            and key not in self._explicit_operations
        )

    def discard(self, key: ClaimKey) -> None:
        """Forget completed ownership after deliberate deletion."""
        self.automatic_cleanup_claims.discard(key)
        self.automatic_cleanup_claim_uids.pop(key, None)
        self.caller_owned_claims.discard(key)
        state = self._explicit_operations.pop(key, None)
        if state is not None:
            state.invalidated = True
            state.generated_cleanup_pending = False
            state.generated_cleanup_uid = None
        for operation in self._lookup_operations.get(key, []):
            operation.invalidated = True

    def _restore_previous_ownership(
        self, key: ClaimKey, state: ExplicitClaimOperations
    ) -> None:
        if state.caller_owned_before:
            self.caller_owned_claims.add(key)
        else:
            self.caller_owned_claims.discard(key)

        if state.automatic_cleanup_before:
            self.automatic_cleanup_claims.add(key)
        elif state.caller_owned_before:
            self.automatic_cleanup_claims.discard(key)
            self.automatic_cleanup_claim_uids.pop(key, None)
        # If neither owner existed before, preserve an automatic registration
        # made by a generated operation while this explicit operation ran.
