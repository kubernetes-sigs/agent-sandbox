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

"""LeaseLock speaks the protocol kubernetes.leaderelection drives, against a
coordination.k8s.io Lease instead of a ConfigMap annotation."""

from __future__ import annotations

import copy
import datetime as _dt
import json

import pytest
from kubernetes import client
from kubernetes.client.exceptions import ApiException
from kubernetes.leaderelection.leaderelectionrecord import LeaderElectionRecord

from agent_sandbox_fleet.leaselock import LeaseLock


def _api_exc(status: int) -> ApiException:
    e = ApiException(status=status, reason={404: "Not Found", 409: "Conflict",
                                             403: "Forbidden"}[status])
    # The election loop reads json.loads(e.body)["code"] to tell 404 apart.
    e.body = json.dumps({"code": status})
    return e


class FakeCoordination:
    """In-memory CoordinationV1Api with resourceVersion conflict semantics."""

    def __init__(self):
        self.leases: dict[tuple[str, str], client.V1Lease] = {}
        self._rv = 0
        self.fail_reads_with: ApiException | None = None

    def _stamp(self, body):
        self._rv += 1
        body.metadata.resource_version = str(self._rv)
        return copy.deepcopy(body)

    def read_namespaced_lease(self, name, namespace):
        if self.fail_reads_with is not None:
            raise self.fail_reads_with
        try:
            return copy.deepcopy(self.leases[(namespace, name)])
        except KeyError:
            raise _api_exc(404) from None

    def create_namespaced_lease(self, namespace, body):
        key = (namespace, body.metadata.name)
        if key in self.leases:
            raise _api_exc(409)
        self.leases[key] = self._stamp(body)
        return copy.deepcopy(self.leases[key])

    def replace_namespaced_lease(self, name, namespace, body):
        key = (namespace, name)
        cur = self.leases.get(key)
        if cur is None:
            raise _api_exc(404)
        if body.metadata.resource_version != cur.metadata.resource_version:
            raise _api_exc(409)
        self.leases[key] = self._stamp(body)
        return copy.deepcopy(self.leases[key])


def _record(identity="pod-a"):
    now = _dt.datetime.fromtimestamp(1_790_000_000.123456)
    return LeaderElectionRecord(identity, "15", str(now), str(now))


def test_get_on_a_missing_lease_reports_not_found_the_way_the_loop_expects():
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=FakeCoordination())
    status, err = lock.get("fleet-member", "ns")
    assert status is False
    assert json.loads(err.body)["code"] == 404


def test_create_then_get_round_trips_the_record():
    api = FakeCoordination()
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    rec = _record()
    assert lock.create("fleet-member", "ns", rec) is True
    status, got = lock.get("fleet-member", "ns")
    assert status is True
    assert got.holder_identity == "pod-a"
    assert got.lease_duration == "15"
    assert got.acquire_time == rec.acquire_time, "timestamps must round-trip verbatim"
    assert got.renew_time == rec.renew_time
    stored = api.leases[("ns", "fleet-member")].spec
    assert stored.lease_duration_seconds == 15
    assert stored.renew_time.tzinfo is not None, "stored as an aware timestamp"


def test_update_after_get_renews_in_place():
    api = FakeCoordination()
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    lock.create("fleet-member", "ns", _record())
    lock.get("fleet-member", "ns")
    renewed = _record()
    renewed.renew_time = str(_dt.datetime.fromtimestamp(1_790_000_010))
    assert lock.update("fleet-member", "ns", renewed) is True
    _, got = lock.get("fleet-member", "ns")
    assert got.renew_time == renewed.renew_time


def test_a_concurrent_writer_makes_update_return_false():
    # Two members, one Lease. The one holding a stale resourceVersion loses,
    # and the loop re-reads rather than overwriting the other's renew.
    api = FakeCoordination()
    a = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    b = LeaseLock("fleet-member", "ns", "pod-b", api=api)
    a.create("fleet-member", "ns", _record("pod-a"))
    a.get("fleet-member", "ns")
    b.get("fleet-member", "ns")
    assert b.update("fleet-member", "ns", _record("pod-b")) is True
    assert a.update("fleet-member", "ns", _record("pod-a")) is False


def test_transport_errors_are_a_failed_attempt_not_a_crash():
    # urllib3 errors are not ApiException and used to escape the election
    # loop into the exit-2 path, restarting the leader on an apiserver blip.
    from urllib3.exceptions import MaxRetryError, ReadTimeoutError
    api = FakeCoordination()
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    lock.create("fleet-member", "ns", _record())
    api.fail_reads_with = ReadTimeoutError(None, "/leases", "read timed out")
    status, err = lock.get("fleet-member", "ns")
    assert status is False
    assert json.loads(err.body)["code"] != 404, "must not look like 'create me'"
    api.fail_reads_with = None
    # update/create surface the same way
    api.replace_namespaced_lease = lambda *a, **k: (_ for _ in ()).throw(
        MaxRetryError(None, "/leases", "boom"))
    lock.get("fleet-member", "ns")
    assert lock.update("fleet-member", "ns", _record()) is False


def test_rbac_denial_raises_instead_of_retrying_forever():
    api = FakeCoordination()
    api.fail_reads_with = _api_exc(403)
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    with pytest.raises(RuntimeError, match="leases"):
        lock.get("fleet-member", "ns")


def test_the_real_election_loop_elects_exactly_one_holder():
    # Drive kubernetes.leaderelection itself against the fake API: the
    # protocol contract, not just our side of it.
    from kubernetes.leaderelection import electionconfig, leaderelection
    api = FakeCoordination()
    lock = LeaseLock("fleet-member", "ns", "pod-a", api=api)
    cfg = electionconfig.Config(lock, lease_duration=15, renew_deadline=10,
                                retry_period=1, onstarted_leading=lambda: None,
                                onstopped_leading=lambda: None)
    assert leaderelection.LeaderElection(cfg).acquire() is True
    assert api.leases[("ns", "fleet-member")].spec.holder_identity == "pod-a"
    # A second identity sees a live, unexpired lease and cannot take it.
    other = LeaseLock("fleet-member", "ns", "pod-b", api=api)
    cfg2 = electionconfig.Config(other, lease_duration=15, renew_deadline=10,
                                 retry_period=1, onstarted_leading=lambda: None,
                                 onstopped_leading=lambda: None)
    assert leaderelection.LeaderElection(cfg2).try_acquire_or_renew() is False
