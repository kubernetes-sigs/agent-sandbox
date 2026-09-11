#!/usr/bin/env python3
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

import os
import sys
import tempfile
import unittest
import xml.etree.ElementTree as ET

# Make the test importable regardless of how it is invoked (pytest, unittest, etc.)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from scrape_controller_metrics import parse_histogram, write_junit


class TestScrapeControllerMetrics(unittest.TestCase):
    """Unit tests for parse_histogram in scrape_controller_metrics."""

    def test_labels_containing_le_substring(self):
        """Verify that label names containing 'le' as a substring (like 'scale', 'idle') do not cause false matches or ValueError."""
        mock_data = """
# HELP agent_sandbox_claim_controller_startup_latency_ms Latency
# TYPE agent_sandbox_claim_controller_startup_latency_ms histogram
agent_sandbox_claim_controller_startup_latency_ms_bucket{scale="large",idle="false",le="100"} 50
agent_sandbox_claim_controller_startup_latency_ms_bucket{scale="large",idle="false",le="250"} 90
agent_sandbox_claim_controller_startup_latency_ms_bucket{scale="large",idle="false",le="+Inf"} 100
"""
        res = parse_histogram("agent_sandbox_claim_controller_startup_latency_ms", mock_data)
        self.assertIsNotNone(res)
        p50, p90, p99, count = res
        self.assertEqual(count, 100)
        self.assertEqual(p50, 100.0)

    def test_multiple_label_sets_aggregated(self):
        """Verify that bucket counts across multiple label combinations are correctly aggregated."""
        mock_data = """
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="warm",le="100"} 40
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="cold",le="100"} 10
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="warm",le="300"} 80
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="cold",le="300"} 20
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="warm",le="+Inf"} 80
agent_sandbox_claim_controller_startup_latency_ms_bucket{launch_type="cold",le="+Inf"} 20
"""
        res = parse_histogram("agent_sandbox_claim_controller_startup_latency_ms", mock_data)
        self.assertIsNotNone(res)
        p50, p90, p99, count = res
        self.assertEqual(count, 100)
        self.assertEqual(p50, 100.0)
        self.assertAlmostEqual(p90, 260.0)

    def test_empty_or_zero_counts(self):
        """Verify that empty data or 0-count histograms return None."""
        mock_data = """
agent_sandbox_claim_controller_startup_latency_ms_bucket{le="100"} 0
agent_sandbox_claim_controller_startup_latency_ms_bucket{le="+Inf"} 0
"""
        self.assertIsNone(parse_histogram("agent_sandbox_claim_controller_startup_latency_ms", mock_data))
        self.assertIsNone(parse_histogram("agent_sandbox_claim_controller_startup_latency_ms", ""))


class TestWriteJunit(unittest.TestCase):
    """Unit tests for the JUnit report emitted for perf-gate results."""

    def test_all_passed(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "junit_controller-metrics.xml")
            write_junit(path, [
                ("claim-adoption-latency-p50", None),
                ("claim-adoption-latency-p99", None),
            ])
            suite = ET.parse(path).getroot()
            self.assertEqual(suite.tag, "testsuite")
            self.assertEqual(suite.get("tests"), "2")
            self.assertEqual(suite.get("failures"), "0")
            cases = suite.findall("testcase")
            self.assertEqual(len(cases), 2)
            for case in cases:
                self.assertIsNone(case.find("failure"))

    def test_failure_message_contains_measured_value_and_target(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "junit_controller-metrics.xml")
            msg = "P99: ~562.5 ms exceeded target <= 500.0 ms"
            write_junit(path, [
                ("claim-adoption-latency-p50", None),
                ("claim-adoption-latency-p99", msg),
            ])
            suite = ET.parse(path).getroot()
            self.assertEqual(suite.get("tests"), "2")
            self.assertEqual(suite.get("failures"), "1")
            failed = suite.find("testcase[@name='claim-adoption-latency-p99']")
            self.assertIsNotNone(failed)
            failure = failed.find("failure")
            self.assertIsNotNone(failure)
            self.assertEqual(failure.get("message"), msg)
            self.assertEqual(failure.text, msg)

    def test_creates_missing_parent_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "artifacts", "junit_controller-metrics.xml")
            write_junit(path, [("claim-adoption-latency", "No metrics recorded")])
            suite = ET.parse(path).getroot()
            self.assertEqual(suite.get("failures"), "1")


if __name__ == "__main__":
    unittest.main()
