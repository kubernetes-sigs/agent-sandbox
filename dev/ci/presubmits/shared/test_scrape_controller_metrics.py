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

from scrape_controller_metrics import main, parse_histogram, write_junit

import http.server
import threading
from unittest import mock


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


class TestMainCLI(unittest.TestCase):
    """End-to-end tests of main(): fetch, validate, exit code, junit."""

    PASSING = (
        'agent_sandbox_claim_controller_startup_latency_ms_bucket{le="100"} 100\n'
        'agent_sandbox_claim_controller_startup_latency_ms_bucket{le="+Inf"} 100\n'
    )
    VIOLATING = (
        'agent_sandbox_claim_controller_startup_latency_ms_bucket{le="100"} 0\n'
        'agent_sandbox_claim_controller_startup_latency_ms_bucket{le="1000"} 100\n'
        'agent_sandbox_claim_controller_startup_latency_ms_bucket{le="+Inf"} 100\n'
    )

    def serve(self, body):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self, _body=body.encode()):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(_body)

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_address[1]}/metrics"

    def run_main(self, url, junit_path):
        argv = ["scrape_controller_metrics.py", "--metrics-url", url,
                "--threshold-50", "300", "--threshold-90", "300",
                "--threshold-99", "500", "--junit-out", junit_path]
        with mock.patch.object(sys, "argv", argv):
            try:
                main()
            except SystemExit as e:
                return e.code or 0
        return 0

    def junit(self, path):
        return ET.parse(path).getroot().find(".")

    def test_violation_exits_nonzero_with_failing_junit(self):
        with tempfile.TemporaryDirectory() as d:
            junit_path = os.path.join(d, "junit.xml")
            code = self.run_main(self.serve(self.VIOLATING), junit_path)
            self.assertEqual(code, 1)
            suite = ET.parse(junit_path).getroot()
            self.assertEqual(suite.get("failures"), "3")
            messages = [f.get("message") for f in suite.iter("failure")]
            self.assertTrue(any("exceeded target" in m for m in messages))

    def test_metrics_unavailable_exits_nonzero_with_failing_junit(self):
        with tempfile.TemporaryDirectory() as d:
            junit_path = os.path.join(d, "junit.xml")
            # Nothing listens on port 1; the fetch fails immediately.
            code = self.run_main("http://127.0.0.1:1/metrics", junit_path)
            self.assertEqual(code, 1)
            suite = ET.parse(junit_path).getroot()
            self.assertEqual(suite.get("failures"), "1")
            (failure,) = list(suite.iter("failure"))
            self.assertIn("Could not fetch metrics", failure.get("message"))

    def test_passing_run_exits_zero_with_clean_junit(self):
        with tempfile.TemporaryDirectory() as d:
            junit_path = os.path.join(d, "junit.xml")
            code = self.run_main(self.serve(self.PASSING), junit_path)
            self.assertEqual(code, 0)
            suite = ET.parse(junit_path).getroot()
            self.assertEqual(suite.get("failures"), "0")
            self.assertEqual(suite.get("tests"), "3")


if __name__ == "__main__":
    unittest.main()
