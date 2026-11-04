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
"""Unit tests for the batch reaper."""

import tempfile
import unittest
from datetime import UTC, datetime, timedelta
from unittest.mock import MagicMock, patch

from kubernetes import client as k8s_client

from k8s_agent_sandbox import batch_reaper
from k8s_agent_sandbox.constants import BATCH_ID_LABEL

MARGIN = 60


def _lease(batch_id="b1", age=0, duration=60, resource_version="10", name=None):
    """A batch Lease last renewed ``age`` seconds ago."""
    return k8s_client.V1Lease(
        metadata=k8s_client.V1ObjectMeta(
            name=name or f"batch-{batch_id}",
            labels={BATCH_ID_LABEL: batch_id},
            resource_version=resource_version,
        ),
        spec=k8s_client.V1LeaseSpec(
            renew_time=datetime.now(UTC) - timedelta(seconds=age),
            lease_duration_seconds=duration,
        ),
    )


def _page(leases, continue_token=None):
    return k8s_client.V1LeaseList(
        items=leases, metadata=k8s_client.V1ListMeta(_continue=continue_token)
    )


class TestReap(unittest.TestCase):

    def setUp(self):
        self.helper = MagicMock()
        self.leases: dict[str, list] = {"default": []}
        self.helper.coordination_v1_api.list_namespaced_lease.side_effect = (
            lambda namespace, **kwargs: _page(self.leases[namespace])
        )
        time_patch = patch.object(batch_reaper, "time")
        self.time = time_patch.start()
        self.time.monotonic.return_value = 0.0
        self.addCleanup(time_patch.stop)

    def reap(self, namespaces=("default",), dry_run=False):
        return batch_reaper.reap(self.helper, list(namespaces), MARGIN, dry_run)

    def deleted_leases(self):
        return [
            (c.args[0], c.args[1], c.kwargs["body"].preconditions.resource_version)
            for c in self.helper.coordination_v1_api.delete_namespaced_lease.call_args_list
        ]

    def test_reaps_only_leases_stale_past_duration_plus_margin(self):
        # A 60s Lease is reapable 60s + MARGIN after its last renewal, with a few seconds of slack
        # either side for the test's own clock.
        self.leases["default"] = [
            _lease("fresh", age=0),
            _lease("within-margin", age=60 + MARGIN - 5),
            _lease("stale", age=60 + MARGIN + 5, resource_version="42"),
            # A detached batch is fresh until its grace runs out.
            _lease("detached", age=100, duration=300),
            _lease("detach-expired", age=30 + MARGIN + 5, duration=30),
        ]
        reaped, errors = self.reap()
        self.assertEqual(reaped, [("default", "detach-expired"), ("default", "stale")])
        self.assertEqual(errors, 0)
        self.assertEqual(
            self.deleted_leases(),
            [("batch-detach-expired", "default", "10"), ("batch-stale", "default", "42")],
        )

    def test_never_touches_a_lease_that_isnt_a_valid_batch_lease(self):
        self.leases["default"] = [
            _lease("Not-Valid", age=1000),
            _lease("b1", age=1000, name="other-name"),
            _lease("b2", age=1000, resource_version=None),
        ]
        self.assertEqual(self.reap(), ([], 0))
        self.helper.coordination_v1_api.delete_namespaced_lease.assert_not_called()

    def test_waits_each_leases_renew_interval_then_deletes_only_the_leases(self):
        self.leases["default"] = [_lease("b2", age=1000, duration=90)]
        self.leases["other"] = [_lease("b1", age=1000, duration=60)]
        calls = []
        clock = [100.0]

        def sleep(seconds):
            calls.append(("sleep", seconds))
            clock[0] += seconds

        self.time.monotonic.side_effect = lambda: clock[0]
        self.time.sleep.side_effect = sleep
        api = self.helper.coordination_v1_api
        api.delete_namespaced_lease.side_effect = lambda name, *a, **k: calls.append(("lease", name))

        reaped, _ = self.reap(["default", "other"])
        self.assertEqual(reaped, [("other", "b1"), ("default", "b2")])
        # Each Lease waits out its own renew interval from the end of the lists, 60 // 3 then
        # 90 // 3, so the longer one doesn't delay the shorter one.
        self.assertEqual(
            calls, [("sleep", 20), ("lease", "batch-b1"), ("sleep", 10), ("lease", "batch-b2")]
        )
        # The garbage collector deletes the claims, so the reaper makes no other request.
        self.assertEqual(
            {name for name, _, _ in self.helper.mock_calls},
            {"coordination_v1_api.list_namespaced_lease", "coordination_v1_api.delete_namespaced_lease"},
        )

    def test_lease_delete_outcomes(self):
        for status, reaped, errors in (
            # Renewed, detached or taken over since it was read.
            (409, [], 0),
            # Already deleted by release() or another reaper.
            (404, [("default", "b1")], 0),
            (500, [], 1),
        ):
            with self.subTest(status=status):
                self.helper.reset_mock()
                self.leases["default"] = [_lease("b1", age=1000)]
                self.helper.coordination_v1_api.delete_namespaced_lease.side_effect = (
                    k8s_client.ApiException(status=status)
                )
                self.assertEqual(self.reap(), (reaped, errors))

    def test_a_namespace_that_fails_to_list_is_skipped_and_counted(self):
        self.leases["other"] = [_lease("b2", age=1000)]
        lister = self.helper.coordination_v1_api.list_namespaced_lease

        def list_leases(namespace, **kwargs):
            if namespace == "forbidden":
                raise k8s_client.ApiException(status=403)
            return _page(self.leases[namespace])

        lister.side_effect = list_leases
        self.assertEqual(self.reap(["forbidden", "other"]), ([("other", "b2")], 1))

    def test_lists_every_page(self):
        pages = iter([_page([_lease("b1", age=1000)], "token"), _page([_lease("b2", age=1000)])])
        lister = self.helper.coordination_v1_api.list_namespaced_lease
        lister.side_effect = lambda namespace, **kwargs: next(pages)
        reaped, _ = self.reap(dry_run=True)
        self.assertEqual(reaped, [("default", "b1"), ("default", "b2")])
        first, second = (c.kwargs for c in lister.call_args_list)
        self.assertEqual(first["label_selector"], BATCH_ID_LABEL)
        self.assertIsNone(first["_continue"])
        self.assertEqual(second["_continue"], "token")
        # A consistent list, so a Lease's resourceVersion is never older than the latest write.
        self.assertNotIn("resource_version", first)
        # A dry run neither waits nor deletes.
        self.time.sleep.assert_not_called()
        self.helper.coordination_v1_api.delete_namespaced_lease.assert_not_called()


class TestMain(unittest.TestCase):

    @patch.object(batch_reaper, "K8sHelper")
    @patch.object(batch_reaper, "reap")
    def test_flags_and_exit_code(self, reap, _):
        for errors, code in ((0, 0), (2, 1)):
            with self.subTest(errors=errors):
                reap.return_value = ([], errors)
                self.assertEqual(
                    batch_reaper.main(["--namespaces", "a, b", "--skew-margin", "30", "--dry-run"]), code
                )
                self.assertEqual(reap.call_args.args[1:], (["a", "b"], 30, True))
        # Like client-go, POD_NAMESPACE wins over the service account's namespace file.
        with tempfile.NamedTemporaryFile("w") as namespace_file:
            namespace_file.write("from-file\n")
            namespace_file.flush()
            for env, path, namespace in (
                ({"POD_NAMESPACE": "own"}, namespace_file.name, "own"),
                ({}, namespace_file.name, "from-file"),
                ({}, "/nonexistent", "default"),
            ):
                with (
                    self.subTest(namespace=namespace),
                    patch.dict("os.environ", env, clear=True),
                    patch.object(batch_reaper, "_SERVICE_ACCOUNT_NAMESPACE_PATH", path),
                ):
                    batch_reaper.main([])
                    self.assertEqual(
                        reap.call_args.args[1:],
                        ([namespace], batch_reaper.REAPER_DEFAULT_SKEW_MARGIN_SECONDS, False),
                    )
        # The margin can't be below the SDK's own, or the reaper could act before get_batch stops.
        with self.assertRaises(SystemExit), patch("sys.stderr"):
            batch_reaper.main(["--skew-margin", "4"])


if __name__ == "__main__":
    unittest.main()
