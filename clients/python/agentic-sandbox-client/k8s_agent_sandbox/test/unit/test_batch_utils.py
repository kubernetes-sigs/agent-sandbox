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
from datetime import UTC, datetime, timedelta

from k8s_agent_sandbox import batch_utils
from k8s_agent_sandbox.batch_utils import (
    BATCH_DEFAULT_LEASE_DURATION_SECONDS,
    CLOCK_SKEW_MARGIN,
)
from k8s_agent_sandbox.constants import BATCH_ID_LABEL
from k8s_agent_sandbox.exceptions import BatchError
from k8s_agent_sandbox.models import BatchGroup


class TestBatchIdValidation(unittest.TestCase):

    def test_valid_ids_accepted(self):
        for batch_id in ("b1234567890", "a", "abc-123"):
            batch_utils.validate_batch_id(batch_id)

    def test_rejects_non_letter_start(self):
        with self.assertRaises(ValueError):
            batch_utils.validate_batch_id("1abc")

    def test_rejects_too_long(self):
        with self.assertRaises(ValueError):
            batch_utils.validate_batch_id("a" * (batch_utils.BATCH_ID_MAX_LENGTH + 1))

    def test_rejects_uppercase(self):
        with self.assertRaises(ValueError):
            batch_utils.validate_batch_id("Abc")

    def test_max_length_accepted(self):
        batch_utils.validate_batch_id("a" * batch_utils.BATCH_ID_MAX_LENGTH)


class TestLeaseDurationValidation(unittest.TestCase):

    def test_valid_int_accepted(self):
        self.assertEqual(batch_utils.validate_lease_duration_value(90), 90)

    def test_boundary_equal_to_skew_margin_raises_exact_message(self):
        with self.assertRaises(ValueError) as ctx:
            batch_utils.validate_lease_duration_value(CLOCK_SKEW_MARGIN)
        self.assertEqual(
            str(ctx.exception),
            f"Duration must be greater than clock skew margin ({CLOCK_SKEW_MARGIN}s)",
        )

    def test_invalid_values_raise(self):
        for bad in (1.5, 30.0, True, "30", 0, -5):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    batch_utils.validate_lease_duration_value(bad)


class TestLeaseDurationAnnotation(unittest.TestCase):

    def test_missing_annotation_defaults_to_60(self):
        self.assertEqual(
            batch_utils.parse_lease_duration_annotation(None),
            BATCH_DEFAULT_LEASE_DURATION_SECONDS,
        )

    def test_valid_annotation_parsed(self):
        self.assertEqual(batch_utils.parse_lease_duration_annotation("90"), 90)

    def test_boundary_annotation_of_skew_margin_plus_one_accepted(self):
        self.assertEqual(
            batch_utils.parse_lease_duration_annotation(str(CLOCK_SKEW_MARGIN + 1)),
            CLOCK_SKEW_MARGIN + 1,
        )

    def test_invalid_annotations_raise_batch_error(self):
        for bad in ("abc", "1.5", "0", "-5", "5"):
            with self.subTest(bad=bad):
                with self.assertRaises(BatchError):
                    batch_utils.parse_lease_duration_annotation(bad)


class TestLeaseStaleness(unittest.TestCase):

    def test_boundary_exactly_at_skew_margin_is_not_stale(self):
        now = datetime(2026, 1, 1, tzinfo=UTC)
        renew_time = now - timedelta(
            seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS - CLOCK_SKEW_MARGIN
        )
        self.assertFalse(
            batch_utils.is_lease_stale(renew_time, BATCH_DEFAULT_LEASE_DURATION_SECONDS, now)
        )

    def test_one_second_after_margin_is_stale(self):
        now = datetime(2026, 1, 1, tzinfo=UTC)
        renew_time = now - timedelta(
            seconds=BATCH_DEFAULT_LEASE_DURATION_SECONDS - CLOCK_SKEW_MARGIN + 1
        )
        self.assertTrue(
            batch_utils.is_lease_stale(renew_time, BATCH_DEFAULT_LEASE_DURATION_SECONDS, now)
        )



class TestRetryableStatus(unittest.TestCase):

    def test_table(self):
        cases = {
            None: True,
            429: True,
            500: True,
            503: True,
            599: True,
            400: False,
            403: False,
            404: False,
            409: False,
            410: False,
            600: False,
        }
        for status, want in cases.items():
            with self.subTest(status=status):
                self.assertIs(batch_utils.is_retryable_status(status), want)


class TestBackoffDelay(unittest.TestCase):

    def test_first_attempt_is_between_half_base_and_base(self):
        self.assertEqual(batch_utils.backoff_delay(1, 0.5, 30.0, rand=lambda: 0.0), 0.25)
        self.assertEqual(batch_utils.backoff_delay(1, 0.5, 30.0, rand=lambda: 1.0), 0.5)

    def test_doubles_per_attempt(self):
        self.assertEqual(batch_utils.backoff_delay(2, 0.5, 30.0, rand=lambda: 1.0), 1.0)
        self.assertEqual(batch_utils.backoff_delay(4, 0.5, 30.0, rand=lambda: 1.0), 4.0)
        self.assertEqual(batch_utils.backoff_delay(4, 0.5, 30.0, rand=lambda: 0.0), 2.0)

    def test_capped(self):
        self.assertEqual(batch_utils.backoff_delay(8, 0.5, 30.0, rand=lambda: 1.0), 30.0)
        self.assertEqual(batch_utils.backoff_delay(8, 0.5, 30.0, rand=lambda: 0.0), 15.0)

    def test_large_attempt_does_not_overflow(self):
        self.assertEqual(batch_utils.backoff_delay(10_000, 0.5, 30.0, rand=lambda: 1.0), 30.0)

    def test_doubling_reaches_a_cap_far_above_base(self):
        self.assertEqual(batch_utils.backoff_delay(10, 0.1, 1000.0, rand=lambda: 1.0), 51.2)
        self.assertEqual(batch_utils.backoff_delay(15, 0.1, 1000.0, rand=lambda: 1.0), 1000.0)


def _args(**overrides):
    kwargs = dict(
        groups=[BatchGroup(warmpool="pool-a", size=2)],
        labels=None,
        batch_id=None,
        create_rps=None,
        max_in_flight=None,
        work_budget=None,
        quorum_timeout=None,
        lease_duration=None,
    )
    kwargs.update(overrides)
    return batch_utils.validate_claim_batch_args(**kwargs)


class TestValidateClaimBatchArgs(unittest.TestCase):

    def test_rejects_each_invalid_input(self):
        pool_a = BatchGroup(warmpool="pool-a", size=1)
        cases = {
            "empty groups": {"groups": []},
            "size 0": {"groups": [BatchGroup(warmpool="pool-a", size=0)]},
            "duplicate pool": {"groups": [pool_a, pool_a]},
            "not a BatchGroup": {"groups": [{"warmpool": "pool-a", "size": 1}]},
            "create_rps 0": {"create_rps": 0},
            "create_rps -1": {"create_rps": -1},
            "create_rps True": {"create_rps": True},
            "create_rps str": {"create_rps": "5"},
            "max_in_flight 0": {"max_in_flight": 0},
            "max_in_flight 1.5": {"max_in_flight": 1.5},
            "max_in_flight True": {"max_in_flight": True},
            "work_budget 0": {"work_budget": 0},
            "work_budget 1.5": {"work_budget": 1.5},
            "quorum_timeout 0": {"quorum_timeout": 0},
            "quorum_timeout 1.5": {"quorum_timeout": 1.5},
            "lease_duration 5": {"lease_duration": CLOCK_SKEW_MARGIN},
            "labels set batch id": {"labels": {BATCH_ID_LABEL: "b1"}},
            "invalid label": {"labels": {"bad key!": "v"}},
            "invalid batch_id": {"batch_id": "1abc"},
        }
        for name, override in cases.items():
            with self.subTest(name):
                with self.assertRaises(ValueError):
                    _args(**override)

    def test_fills_defaults(self):
        args = _args()
        self.assertEqual(args.create_rps, batch_utils.BATCH_DEFAULT_CREATE_RPS)
        self.assertEqual(args.max_in_flight, batch_utils.BATCH_DEFAULT_MAX_IN_FLIGHT)
        self.assertEqual(args.work_budget, batch_utils.BATCH_DEFAULT_WORK_BUDGET_SECONDS)
        self.assertEqual(args.quorum_timeout, batch_utils.BATCH_DEFAULT_QUORUM_TIMEOUT_SECONDS)
        self.assertEqual(args.lease_duration, BATCH_DEFAULT_LEASE_DURATION_SECONDS)
        self.assertEqual(args.labels, {})
        batch_utils.validate_batch_id(args.batch_id)


class TestGenerateBatchId(unittest.TestCase):

    def test_generated_id_is_valid_and_random(self):
        first, second = batch_utils.generate_batch_id(), batch_utils.generate_batch_id()
        batch_utils.validate_batch_id(first)
        self.assertEqual(len(first), 12)
        self.assertNotEqual(first, second)


class TestCreateErrorOutcome(unittest.TestCase):

    def test_outcome_table(self):
        last = batch_utils.BATCH_CREATE_ATTEMPTS
        success, retry, fail = (
            batch_utils.CreateOutcome.SUCCESS, batch_utils.CreateOutcome.RETRY, batch_utils.CreateOutcome.FAIL
        )
        cases = [
            (409, 1, fail),
            (409, 2, success),
            (429, 1, retry),
            (503, 1, retry),
            (None, 1, retry),
            (429, last, fail),
            (503, last, fail),
            (None, last, fail),
            (400, 1, fail),
            (403, 1, fail),
            (404, 1, fail),
            (422, 1, fail),
        ]
        for status, attempt, want in cases:
            with self.subTest(status=status, attempt=attempt):
                self.assertEqual(batch_utils.create_error_outcome(status, attempt), want)


class _ErrorWithHeaders(Exception):
    def __init__(self, headers):
        super().__init__("error")
        self.headers = headers


class TestRetryDelay(unittest.TestCase):

    def test_integer_retry_after_wins_and_is_capped(self):
        self.assertEqual(batch_utils.retry_delay(1, _ErrorWithHeaders({"Retry-After": "7"})), 7.0)
        self.assertEqual(
            batch_utils.retry_delay(1, _ErrorWithHeaders({"Retry-After": "600"})),
            batch_utils.BATCH_BACKOFF_MAX_SECONDS,
        )

    def test_missing_or_http_date_retry_after_falls_back_to_backoff(self):
        base = batch_utils.BATCH_BACKOFF_BASE_SECONDS
        for error in (
            None,
            _ErrorWithHeaders(None),
            _ErrorWithHeaders({"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"}),
        ):
            with self.subTest(error=error):
                self.assertTrue(base / 2 <= batch_utils.retry_delay(1, error) <= base)


if __name__ == "__main__":
    unittest.main()
