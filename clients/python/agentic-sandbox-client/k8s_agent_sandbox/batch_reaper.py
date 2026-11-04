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

"""Batch reaper, which cleans up batches whose driver is gone.

A driver that dies without running any code (SIGKILL, OOM kill, node loss) never calls
``release()``, so its batch Lease goes stale. Each run, the reaper finds the batch Leases that have
been stale for longer than a clock skew margin, deletes each one only if it hasn't changed for one
renew interval. The batch's claims carry owner references to the Lease, so the garbage collector
deletes them, and their Sandboxes and Pods, once the Lease is gone. It never writes a Lease and
keeps no state between runs, and everything it does is a delete, so a run that stops partway is
harmless.

Run it as ``python -m k8s_agent_sandbox.batch_reaper``, for example from a CronJob every one to two
minutes with ``concurrencyPolicy: Forbid``.
"""

import argparse
import logging
import os
import sys
import time
from collections.abc import Sequence
from datetime import UTC, datetime, timedelta
from typing import NamedTuple

from kubernetes import client
from kubernetes.client.exceptions import ApiException

from . import batch_utils
from .constants import BATCH_ID_LABEL, BATCH_LEASE_NAME_PREFIX
from .k8s_helper import K8sHelper

logger = logging.getLogger(__name__)

REAPER_DEFAULT_SKEW_MARGIN_SECONDS = 60
_LEASE_LIST_PAGE_SIZE = 500
_SERVICE_ACCOUNT_NAMESPACE_PATH = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"


class StaleLease(NamedTuple):
    """A batch Lease the reaper found stale, with the resourceVersion it read."""
    batch_id: str
    resource_version: str
    renew_interval: int


def is_lease_reapable(
    renew_time: datetime, lease_duration_seconds: int, now: datetime, skew_margin: int
) -> bool:
    """Whether a batch Lease is stale enough for the reaper to delete it.

    Stale means ``now > renew_time + lease_duration_seconds + skew_margin``. The margin is added,
    unlike in ``batch_utils.is_lease_stale``, so the driver has given up on the Lease before the
    reaper acts even when the two clocks disagree by up to ``skew_margin``.
    """
    return now > renew_time + timedelta(seconds=lease_duration_seconds + skew_margin)


def _batch_id_of(metadata: client.V1ObjectMeta) -> str | None:
    """Returns the batch id of a batch Lease, or ``None`` if the Lease isn't one the reaper may
    touch. Its batch-id label must be a valid batch id and its name must be ``batch-<id>``, since
    RBAC can't limit the reaper's deletes to batch objects.
    """
    batch_id = (metadata.labels or {}).get(BATCH_ID_LABEL)
    if batch_id is None:
        return None
    try:
        batch_utils.validate_batch_id(batch_id)
    except ValueError:
        return None
    if metadata.name != f"{BATCH_LEASE_NAME_PREFIX}{batch_id}":
        return None
    return batch_id


def find_stale_leases(
    helper: K8sHelper, namespace: str, now: datetime, skew_margin: int
) -> list[StaleLease]:
    """Lists the namespace's batch Leases and returns the stale ones.

    The list is paged, and every delete is conditioned on the resourceVersion read here.
    """
    stale: list[StaleLease] = []
    continue_token: str | None = None
    while True:
        page = helper.coordination_v1_api.list_namespaced_lease(
            namespace,
            label_selector=BATCH_ID_LABEL,
            limit=_LEASE_LIST_PAGE_SIZE,
            _continue=continue_token,
            _request_timeout=batch_utils.BATCH_COLLECTION_REQUEST_TIMEOUT_SECONDS,
        )
        for lease in page.items:
            metadata, spec = lease.metadata, lease.spec
            batch_id = _batch_id_of(metadata) if metadata is not None else None
            # Without a resourceVersion the delete would have no precondition.
            if metadata is None or batch_id is None or not metadata.resource_version:
                continue
            if spec is None or spec.renew_time is None or not spec.lease_duration_seconds:
                # Waiting is always safe, while guessing the expiry could delete live work.
                logger.warning(
                    f"Batch Lease '{metadata.name}' in namespace '{namespace}' has no renewTime or "
                    f"leaseDurationSeconds, skipping"
                )
                continue
            if is_lease_reapable(spec.renew_time, spec.lease_duration_seconds, now, skew_margin):
                stale.append(StaleLease(
                    batch_id=batch_id,
                    resource_version=metadata.resource_version,
                    renew_interval=max(1, spec.lease_duration_seconds // 3),
                ))
        continue_token = page.metadata._continue if page.metadata else None
        if not continue_token:
            return stale


def _delete_lease_if_unchanged(helper: K8sHelper, namespace: str, lease: StaleLease) -> bool:
    """Deletes a stale Lease if its resourceVersion is still the one the reaper read. Returns
    whether the Lease is gone, which is ``False`` only if the Lease changed.
    """
    try:
        helper.coordination_v1_api.delete_namespaced_lease(
            f"{BATCH_LEASE_NAME_PREFIX}{lease.batch_id}",
            namespace,
            body=client.V1DeleteOptions(
                preconditions=client.V1Preconditions(resource_version=lease.resource_version)
            ),
            _request_timeout=batch_utils.BATCH_REQUEST_TIMEOUT_SECONDS,
        )
    except ApiException as e:
        if e.status == 409:
            # A renewal, detach() or takeover wrote the Lease after the reaper read it.
            logger.info(f"Batch '{lease.batch_id}' in namespace '{namespace}' renewed its Lease, skipping")
            return False
        # release() or another reaper already deleted it.
        if e.status != 404:
            raise
    return True


def reap(
    helper: K8sHelper,
    namespaces: Sequence[str],
    skew_margin: int = REAPER_DEFAULT_SKEW_MARGIN_SECONDS,
    dry_run: bool = False,
) -> tuple[list[tuple[str, str]], int]:
    """Reaps the stale batches in each namespace.

    Lists each namespace's batch Leases, then deletes each stale Lease once one of its own renew
    intervals has passed since the lists, so a live driver whose clock runs behind has renewed by
    then. Leases are deleted shortest interval first, so a Lease with a long duration never holds
    back one with a short duration, and the run lasts the longest interval whatever the number of
    namespaces. Each delete has a resourceVersion precondition, and a batch whose Lease changed is
    skipped. It deletes no claims itself. The garbage collector deletes them through their owner
    references, matching the owner by UID, so it never touches a newer batch that reuses the id.

    A namespace whose Leases can't be listed, for example because the reaper has no RoleBinding
    there, is logged and skipped, and so is a Lease that fails to delete.

    Returns:
        The ``(namespace, batch_id)`` of each batch it reaped, or would reap with ``dry_run``, and
        how many errors it logged.
    """
    stale: list[tuple[str, StaleLease]] = []
    errors = 0
    for namespace in namespaces:
        try:
            found = find_stale_leases(helper, namespace, datetime.now(UTC), skew_margin)
        except Exception as e:
            logger.error(f"Failed to list batch Leases in namespace '{namespace}': {e}")
            errors += 1
            continue
        stale.extend((namespace, lease) for lease in found)
    if dry_run:
        for namespace, lease in stale:
            logger.info(f"Would reap batch '{lease.batch_id}' in namespace '{namespace}'")
        return [(namespace, lease.batch_id) for namespace, lease in stale], errors
    if not stale:
        return [], errors

    # Staleness compares the driver's clock with the reaper's, so a live driver whose clock runs
    # behind looks stale. A live driver renews within one renew interval, which changes the Lease's
    # resourceVersion and makes the delete below fail its precondition. Only a dead driver's Lease is
    # still unchanged. The wait is counted from after the lists returned, so it covers each read.
    listed_at = time.monotonic()
    reaped: list[tuple[str, str]] = []
    for namespace, lease in sorted(stale, key=lambda item: item[1].renew_interval):
        time.sleep(max(0.0, listed_at + lease.renew_interval - time.monotonic()))
        try:
            if not _delete_lease_if_unchanged(helper, namespace, lease):
                continue
        except Exception as e:
            logger.error(f"Failed to reap batch '{lease.batch_id}' in namespace '{namespace}': {e}")
            errors += 1
            continue
        logger.info(f"Reaped batch '{lease.batch_id}' in namespace '{namespace}'")
        reaped.append((namespace, lease.batch_id))
    return reaped, errors


def _default_namespace() -> str:
    # The same lookup as client-go's in-cluster config, which prefers a POD_NAMESPACE set through
    # the downward API over the service account's namespace file.
    if namespace := os.environ.get("POD_NAMESPACE"):
        return namespace
    try:
        with open(_SERVICE_ACCOUNT_NAMESPACE_PATH) as f:
            return f.read().strip()
    except OSError:
        return "default"


def _skew_margin(value: str) -> int:
    margin = int(value)
    if margin < batch_utils.CLOCK_SKEW_MARGIN:
        raise argparse.ArgumentTypeError(
            f"must be at least the SDK's clock skew margin ({batch_utils.CLOCK_SKEW_MARGIN}s)"
        )
    return margin


def main(argv: Sequence[str] | None = None) -> int:
    """Runs the reaper once over each namespace and returns the exit code, 1 if any error was
    logged and 0 otherwise.
    """
    parser = argparse.ArgumentParser(
        prog="python -m k8s_agent_sandbox.batch_reaper",
        description="Deletes stale batch Leases, leaving their claims to the garbage collector.",
    )
    parser.add_argument(
        "--namespaces",
        help="Comma-separated namespaces to reap. Defaults to the reaper's own namespace.",
    )
    parser.add_argument(
        "--skew-margin",
        type=_skew_margin,
        default=REAPER_DEFAULT_SKEW_MARGIN_SECONDS,
        help="Seconds a Lease must be stale beyond its duration before it is reaped.",
    )
    parser.add_argument(
        "--dry-run", action="store_true", help="Log the batches that would be reaped, and delete nothing."
    )
    args = parser.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

    namespaces = [ns.strip() for ns in (args.namespaces or _default_namespace()).split(",") if ns.strip()]
    _, errors = reap(K8sHelper(), namespaces, args.skew_margin, args.dry_run)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
