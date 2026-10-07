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

"""A `coordination.k8s.io` Lease lock for the kubernetes client's election loop.

The `kubernetes` Python client ships `kubernetes.leaderelection` with a single
lock implementation, `ConfigMapLock`, which stores the election record in a
ConfigMap annotation. Leases are the current upstream convention for leader
election (ConfigMap and Endpoints locks are the deprecated path in
client-go), and a Lease is what an operator expects to find when they ask
"who is the active member on this cluster". This class speaks the same
four-method protocol the election loop drives -- `get`, `create`, `update`,
plus `name` / `namespace` / `identity` attributes -- against a `Lease`.

Record values stay strings on the way in and out because that is what the
election loop produces and compares (`str(datetime)` for the two timestamps,
`str(int)` for the duration); the Lease stores them as typed fields and this
class converts at the boundary.

Two deliberate deviations from `ConfigMapLock`. A 401/403 on the Lease is
raised, not swallowed. And urllib3 transport errors (`MaxRetryError`,
`ReadTimeoutError` -- the client does not wrap these in ApiException) are
treated as a failed attempt, not a crash: the election loop retries inside
its renew deadline, so an apiserver blip costs a retry rather than a leader
restart. The election loop treats a failed `get` as "retry
later" forever, which for a missing RBAC rule means a member that never
leads and never says why. Raising makes the pod crash-loop with the rule it
needs in its logs.
"""

from __future__ import annotations

import datetime as _dt
import logging
from typing import Any

from kubernetes import client
from kubernetes.client.exceptions import ApiException
from kubernetes.leaderelection.leaderelectionrecord import LeaderElectionRecord
from urllib3.exceptions import HTTPError as _TransportError

log = logging.getLogger("agent_sandbox_fleet.leaselock")

_RBAC_HINT = (
    "the member's ServiceAccount needs get/create/update on leases."
    "coordination.k8s.io in its namespace (deploy/rbac.yaml grants this); "
    "or run with --no-leader-elect and replicas: 1"
)


def _to_datetime(value: str | None) -> _dt.datetime | None:
    """Election-loop timestamp string -> aware UTC datetime for the API."""
    if not value:
        return None
    parsed = _dt.datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        # str(datetime.fromtimestamp(...)) is naive local time.
        parsed = parsed.astimezone()
    return parsed.astimezone(_dt.timezone.utc)


def _to_record_str(value: _dt.datetime | None) -> str | None:
    """API datetime -> the naive-local `str(datetime)` form the loop writes,
    so a leader's own record round-trips equal to what it last wrote."""
    if value is None:
        return None
    return str(value.astimezone().replace(tzinfo=None))


class LeaseLock:
    def __init__(self, name: str, namespace: str, identity: str,
                 api: Any | None = None):
        self.name = name
        self.namespace = namespace
        self.identity = str(identity)
        self.api_instance = api or client.CoordinationV1Api()
        # The last Lease object read, so update() is a read-modify-write on
        # the server's resourceVersion and a concurrent writer gets a 409.
        self._lease: Any | None = None

    # -- protocol the election loop drives --------------------------------

    def get(self, name: str, namespace: str):
        try:
            lease = self.api_instance.read_namespaced_lease(name, namespace)
        except ApiException as e:
            if e.status in (401, 403):
                raise RuntimeError(
                    f"cannot read Lease {namespace}/{name} ({e.status}): "
                    f"{_RBAC_HINT}") from e
            return False, e
        except _TransportError as e:
            log.info("transient error reading lease %s/%s: %s", namespace, name, e)
            return False, _as_api_exception(e)
        self._lease = lease
        spec = lease.spec
        if spec is None or spec.holder_identity is None:
            return True, None
        return True, LeaderElectionRecord(
            spec.holder_identity,
            None if spec.lease_duration_seconds is None
            else str(spec.lease_duration_seconds),
            _to_record_str(spec.acquire_time),
            _to_record_str(spec.renew_time),
        )

    def create(self, name: str, namespace: str,
               election_record: LeaderElectionRecord) -> bool:
        body = client.V1Lease(
            metadata=client.V1ObjectMeta(name=name, namespace=namespace),
            spec=self._spec_from(election_record),
        )
        try:
            self._lease = self.api_instance.create_namespaced_lease(namespace, body)
            return True
        except ApiException as e:
            if e.status in (401, 403):
                raise RuntimeError(
                    f"cannot create Lease {namespace}/{name} ({e.status}): "
                    f"{_RBAC_HINT}") from e
            log.info("failed to create lease %s/%s: %s", namespace, name, e.reason)
            return False
        except _TransportError as e:
            log.info("transient error creating lease %s/%s: %s", namespace, name, e)
            return False

    def update(self, name: str, namespace: str,
               updated_record: LeaderElectionRecord) -> bool:
        if self._lease is None:
            # update() without a prior get() is not something the loop does;
            # fetch rather than guess at a resourceVersion.
            status, _ = self.get(name, namespace)
            if not status or self._lease is None:
                return False
        self._lease.spec = self._spec_from(updated_record)
        try:
            self._lease = self.api_instance.replace_namespaced_lease(
                name, namespace, self._lease)
            return True
        except ApiException as e:
            # 409 == someone else wrote since we read; the loop re-gets.
            log.info("failed to update lease %s/%s: %s", namespace, name, e.reason)
            return False
        except _TransportError as e:
            log.info("transient error updating lease %s/%s: %s", namespace, name, e)
            return False

    # -- helpers -----------------------------------------------------------


    @staticmethod
    def _spec_from(record: LeaderElectionRecord) -> Any:
        return client.V1LeaseSpec(
            holder_identity=record.holder_identity,
            lease_duration_seconds=(None if record.lease_duration is None
                                    else int(record.lease_duration)),
            acquire_time=_to_datetime(record.acquire_time),
            renew_time=_to_datetime(record.renew_time),
        )


def _as_api_exception(err: Exception) -> ApiException:
    """The election loop inspects a failed get's `.body` for a 404 code; give a
    transport error a body it can parse (and that is NOT 404, so it retries
    rather than trying to create)."""
    e = ApiException(status=503, reason=f"transport error: {err}")
    e.body = '{"code": 503}'
    return e
