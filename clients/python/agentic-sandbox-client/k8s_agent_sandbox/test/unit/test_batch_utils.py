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
from k8s_agent_sandbox.exceptions import BatchError


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


if __name__ == "__main__":
    unittest.main()
