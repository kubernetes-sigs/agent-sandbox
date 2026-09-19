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

"""Sync handle for using a claimed or re-attached sandbox batch."""

import logging
import threading
import time
from datetime import UTC, datetime
from typing import TYPE_CHECKING

import urllib3.exceptions
from kubernetes import client
from kubernetes.client.exceptions import ApiException

from . import batch_state, batch_utils
from .constants import BATCH_ID_LABEL, BATCH_LEASE_DURATION_ANNOTATION, BATCH_LEASE_NAME_PREFIX
from .exceptions import (
    BatchError,
    BatchInUseError,
    BatchLeaseExpiredError,
    BatchNotFoundError,
    SandboxNotReadyError,
)
from .models import BatchGroup, Member

if TYPE_CHECKING:
    from .sandbox import Sandbox
    from .sandbox_client import SandboxClient

# Each watch attempt is bounded so the watcher thread periodically rechecks
# for a stop request instead of blocking on the connection indefinitely.
_WATCH_TIMEOUT_SECONDS = 30


def _watch_backoff_delay(failures: int) -> float:
    return batch_utils.backoff_delay(
        failures,
        batch_utils.BATCH_BACKOFF_BASE_SECONDS,
        batch_utils.BATCH_BACKOFF_MAX_SECONDS,
    )


class SandboxBatch:
    """A handle to an existing batch's claims, obtained via ``SandboxClient.get_batch``.

    Keeps a live cache of the batch's claims through one label-scoped watch
    (a background daemon thread), renews the batch Lease on another daemon
    thread, and exposes methods for connecting to ready sandboxes and detaching from the batch.
    """

    def __init__(
        self,
        client: "SandboxClient",
        batch_id: str,
        namespace: str,
        state: batch_state.BatchState,
        lease_name: str,
        holder_identity: str,
        lease_duration: int,
        list_resource_version: str,
    ) -> None:
        self._client = client
        self.batch_id = batch_id
        self.namespace = namespace
        self._state = state
        self._lease_name = lease_name
        self._holder_identity = holder_identity
        self._lease_duration = lease_duration
        self._renew_interval = max(1, lease_duration // 3)
        self._list_resource_version = list_resource_version

        self._lock = threading.Lock()
        self._connected: dict[str, "Sandbox"] = {}
        self._detached = False
        self._lease_released = False
        self._watch_stop = threading.Event()
        self._renew_stop = threading.Event()
        self._renewal_degraded = False
        self._last_renew_success = time.monotonic()
        self._watch_thread: threading.Thread | None = None
        self._renew_thread: threading.Thread | None = None

    @property
    def groups(self) -> list[BatchGroup]:
        """The batch's ``BatchGroup``\\ s; one per warm pool."""
        return list(self._state.groups)

    @property
    def size(self) -> int:
        """The total number of claims across all groups."""
        return self._state.size

    @classmethod
    def _attach(cls, client: "SandboxClient", batch_id: str, namespace: str) -> "SandboxBatch":
        """Attach to an existing batch and take over its Lease."""
        batch_utils.validate_batch_id(batch_id)

        lease_name = f"{BATCH_LEASE_NAME_PREFIX}{batch_id}"
        label_selector = f"{BATCH_ID_LABEL}={batch_id}"

        lease = client.k8s_helper.read_batch_lease(lease_name, namespace)
        claim_items, list_rv = client.k8s_helper.list_sandbox_claim_objects(namespace, label_selector)

        if lease is None and not claim_items:
            raise BatchNotFoundError(f"batch '{batch_id}' not found in namespace '{namespace}'")

        annotation = None
        if lease is not None:
            annotation = (lease.metadata.annotations or {}).get(BATCH_LEASE_DURATION_ANNOTATION)
        duration = batch_utils.parse_lease_duration_annotation(annotation)

        now = datetime.now(UTC)
        spec_duration = lease.spec.lease_duration_seconds if lease is not None else None
        renew_time = lease.spec.renew_time if lease is not None else None
        if (
            lease is None
            or renew_time is None
            or spec_duration is None
            or batch_utils.is_lease_stale(renew_time, spec_duration, now)
        ):
            raise BatchLeaseExpiredError(
                f"batch '{batch_id}' Lease is missing or stale in namespace '{namespace}'"
            )

        if lease.spec.holder_identity is not None:
            raise BatchInUseError(
                f"batch '{batch_id}' is held by '{lease.spec.holder_identity}'"
            )

        groups = batch_state.reconstruct_groups(claim_items)
        state = batch_state.BatchState(batch_id, groups)
        state.seed_from_claims(claim_items)

        holder_identity = batch_utils.generate_holder_identity()
        lease.spec.holder_identity = holder_identity
        lease.spec.renew_time = now
        lease.spec.lease_duration_seconds = duration
        lease.spec.acquire_time = now
        lease.spec.lease_transitions = (lease.spec.lease_transitions or 0) + 1
        try:
            client.k8s_helper.replace_batch_lease(lease_name, namespace, lease)
        except ApiException as e:
            if e.status == 409:
                raise BatchInUseError(
                    f"batch '{batch_id}' Lease was taken over by another client while attaching"
                ) from e
            raise

        handle = cls(
            client=client,
            batch_id=batch_id,
            namespace=namespace,
            state=state,
            lease_name=lease_name,
            holder_identity=holder_identity,
            lease_duration=duration,
            list_resource_version=list_rv,
        )
        handle._start()
        return handle

    def _start(self) -> None:
        self._watch_thread = threading.Thread(target=self._watch_loop, daemon=True)
        self._renew_thread = threading.Thread(target=self._renew_loop, daemon=True)
        self._watch_thread.start()
        self._renew_thread.start()

    def _check_active(self) -> None:
        if self._detached:
            raise BatchError(f"batch '{self.batch_id}' has been detached")

    def members(self, warmpool: str | None = None) -> list[Member]:
        """Returns a snapshot of the batch's members, optionally filtered to one warm pool."""
        with self._lock:
            return self._state.members(warmpool)

    def connect(self, member: Member) -> "Sandbox":
        """Returns a connected ``Sandbox`` for a ready member. If this handle is already connected
        to the Sandbox, it reuses the existing connection, otherwise it creates a new one and caches it for future calls.
        
        Raises ``SandboxNotReadyError`` if the member isn't ready.
        """
        with self._lock:
            self._check_active()
            cached = self._connected.get(member.claim_name)
            if cached is not None:
                return cached
            # No name resolution or existence check as the batch's watch cache already knows the member's sandbox_name
            current = self._state.get_member(member.claim_name)
            if current is None or not current.ready:
                raise SandboxNotReadyError(
                    f"batch member '{member.claim_name}' is not ready"
                )
            sandbox = self._client.sandbox_class(
                claim_name=member.claim_name,
                sandbox_id=current.sandbox_name,
                namespace=self.namespace,
                connection_config=self._client.connection_config,
                tracer_config=self._client.tracer_config,
                k8s_helper=self._client.k8s_helper,
            )
            self._connected[member.claim_name] = sandbox
            return sandbox

    def err(self) -> Exception | None:
        """Returns the error that stopped the batch's watch or background
        Lease renewal (e.g., ``BatchLeaseExpiredError``), or ``None`` while healthy.
        """
        with self._lock:
            return self._state.error()

    def detach(self, grace: int | None = None) -> None:
        """Stops the background watch and Lease renewal, closes cached sandbox connections, and
        releases this handle's hold on the Lease. ``grace`` is the number of seconds to keep the
        Lease valid after detaching; if ``None``, we use the batch's original Lease duration.

        If the Lease release fails, the error propagates and this handle stays in a detached state with
        the Lease still held. Call ``detach`` again to retry; steps that already completed are skipped.
        """
        if grace is not None:
            batch_utils.validate_lease_duration_value(grace)

        # _detached prevents the caller from connecting to Sandboxes, while _lease_released signals that
        # detach fully completed (i.e. the previous steps and the Lease release write succeeded), so callers
        # can retry detach() if the release fails.
        with self._lock:
            if self._lease_released:
                return
            self._detached = True

        self._watch_stop.set()
        self._renew_stop.set()
        if self._renew_thread is not None:
            self._renew_thread.join()

        with self._lock:
            connected = list(self._connected.values())
            self._connected.clear()
        for sandbox in connected:
            try:
                sandbox.close_connection()
            except Exception as e:
                logging.warning(
                    f"Batch '{self.batch_id}' failed to close a cached sandbox connection "
                    f"during detach: {e}"
                )

        lease = self._client.k8s_helper.read_batch_lease(self._lease_name, self.namespace)
        if lease is not None:
            if lease.spec.holder_identity != self._holder_identity:
                logging.info(
                    f"Batch '{self.batch_id}' Lease is held by "
                    f"'{lease.spec.holder_identity}', not this handle; treating as released"
                )
            else:
                lease.spec.holder_identity = None
                lease.spec.renew_time = datetime.now(UTC)
                # Refresh the Lease so it doesn't expire immediately after we release it,
                # giving other clients a chance to acquire it.
                lease.spec.lease_duration_seconds = (
                    grace if grace is not None else self._lease_duration
                )
                self._client.k8s_helper.replace_batch_lease(self._lease_name, self.namespace, lease)

        with self._lock:
            self._lease_released = True

    def _watch_loop(self) -> None:
        """Background watch loop which keeps ``self._state`` in sync with the batch's claims.

        kubernetes_asyncio's informer/reflector helpers don't support custom resources, so
        this manually implements one where the batch state is seeded from a list,
        then kept up to date with watch events using the list's resourceVersion so no events are missed.
        """
        rv = self._list_resource_version
        label_selector = f"{BATCH_ID_LABEL}={self.batch_id}"
        relist = False
        # Consecutive failed attempts, which set the retry backoff.
        failures = 0
        while not self._watch_stop.is_set():
            try:
                if relist:
                    items, rv = self._client.k8s_helper.list_sandbox_claim_objects(
                        self.namespace, label_selector
                    )
                    if self._watch_stop.is_set():
                        return
                    with self._lock:
                        self._state.resync_from_list(items)
                    relist = False
                for event in self._client.k8s_helper.watch_sandbox_claims(
                    self.namespace, label_selector, rv, _WATCH_TIMEOUT_SECONDS
                ):
                    if self._watch_stop.is_set():
                        break
                    failures = 0
                    event_type = event.get("type")
                    obj = event.get("object") or {}
                    seen_rv = (obj.get("metadata") or {}).get("resourceVersion")
                    if seen_rv:
                        rv = seen_rv
                    if event_type == "BOOKMARK":
                        continue
                    with self._lock:
                        if event_type == "DELETED":
                            self._state.mark_lost((obj.get("metadata") or {}).get("name", ""))
                        elif event_type in ("ADDED", "MODIFIED"):
                            self._state.upsert_claim(obj)
                # The watch ended at its timeout, which is not a failure.
                failures = 0
            except client.ApiException as e:
                if e.status == 410:
                    # Indicates etcd RV compaction aged our resourceVersion out of the apiserver's watch cache,
                    # so the watch can't continue. Re-list, reconcile the cache (catches events that occurred while there
                    # was no watch), and continue watching from the new list's resourceVersion.
                    relist = True
                    continue
                if batch_utils.is_retryable_status(e.status):
                    # Likely transient: retry the re-list, or the watch from the last-seen resourceVersion.
                    failures += 1
                    self._watch_stop.wait(_watch_backoff_delay(failures))
                    continue
                # Response codes outside of this (e.g. 403, 404) are not recoverable
                # by retrying, so we error then exit
                if self._watch_stop.is_set():
                    return
                with self._lock:
                    self._state.note_error(e)
                return
            except (
                urllib3.exceptions.ProtocolError,
                urllib3.exceptions.ReadTimeoutError,
                ConnectionError,
            ):
                # Likely transient network-level failure so retry
                failures += 1
                self._watch_stop.wait(_watch_backoff_delay(failures))
                continue
            except Exception as e:
                # Anything else (e.g. an SSL error) is not known to be transient, so surface it
                # via err() instead of leaving the thread dead with err() still None.
                if self._watch_stop.is_set():
                    return
                with self._lock:
                    self._state.note_error(e)
                return

    def _renew_loop(self) -> None:
        """Background lease renewal loop: renews at roughly 1/3 of lease duration
        so a couple of missed renewals in a row still leave margin before the Lease goes stale.
        """
        while not self._renew_stop.wait(self._renew_interval):
            if not self._renew_once():
                return

    def _renew_once(self) -> bool:
        """Does a single attempt to renew a Lease. A transient failure sets ``_renewal_degraded``
        and logs, and if the Lease has not been successfully renewed for a full Lease duration,
        sets a batch-level error.

        Returns ``False`` if another holder has taken the Lease or the Lease no longer exists,
        meaning this handle must stop renewing rather than overwrite it.
        """
        try:
            # Neither Kubernetes client has a default request timeout. Bounding each request by the
            # renewal interval makes a hung apiserver count as a failed renewal.
            lease = self._client.k8s_helper.read_batch_lease(
                self._lease_name, self.namespace, _request_timeout=self._renew_interval
            )
            if lease is None:
                with self._lock:
                    self._state.note_error(
                        BatchLeaseExpiredError(
                            f"batch '{self.batch_id}' Lease '{self._lease_name}' no longer exists"
                        )
                    )
                return False
            if lease.spec.holder_identity != self._holder_identity:
                with self._lock:
                    self._state.note_error(
                        BatchInUseError(
                            f"batch '{self.batch_id}' Lease is no longer held by this handle "
                            f"(current holder: {lease.spec.holder_identity!r})"
                        )
                    )
                return False
            lease.spec.holder_identity = self._holder_identity
            lease.spec.renew_time = datetime.now(UTC)
            lease.spec.lease_duration_seconds = self._lease_duration
            self._client.k8s_helper.replace_batch_lease(
                self._lease_name, self.namespace, lease, _request_timeout=self._renew_interval
            )
        except Exception as e:
            with self._lock:
                if not self._renewal_degraded:
                    logging.info(f"Batch '{self.batch_id}' lease renewal degraded: {e}")
                    self._renewal_degraded = True
                # time.monotonic() is a clock that only ever moves forward, so unlike datetime.now(),
                # it can't jump backward from an NTP correction or a system clock change.
                if time.monotonic() - self._last_renew_success >= self._lease_duration:
                    self._state.note_error(
                        BatchLeaseExpiredError(
                            f"batch '{self.batch_id}' lease has not renewed successfully "
                            f"for {self._lease_duration}s"
                        )
                    )
            return True
        with self._lock:
            self._last_renew_success = time.monotonic()
            self._renewal_degraded = False
        return True
