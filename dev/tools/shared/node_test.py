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
import unittest
import urllib.error
from unittest import mock

# Make the test importable regardless of how it is invoked (python -m unittest,
# pytest from any cwd, etc.).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import node


class InstallNodeDownloadRetryTest(unittest.TestCase):
    """Regression test: a transient download failure must not fail the whole
    install on the first attempt, since presubmit-test-unit was seen failing
    before any test ran due to a single dropped connection to the nodejs.org
    CDN."""

    def setUp(self):
        patcher = mock.patch("node.platform.system", return_value="Linux")
        self.addCleanup(patcher.stop)
        patcher.start()

        patcher = mock.patch("node.platform.machine", return_value="x86_64")
        self.addCleanup(patcher.stop)
        patcher.start()

        # Avoid sleeping for real during retries; kept so tests can assert on
        # the retry-delay calls.
        patcher = mock.patch("node.time.sleep")
        self.addCleanup(patcher.stop)
        self.sleep = patcher.start()

        # Skip touching the real filesystem for the tar extraction path.
        patcher = mock.patch("node.os.makedirs")
        self.addCleanup(patcher.stop)
        patcher.start()

        patcher = mock.patch("node.os.path.exists", return_value=True)
        self.addCleanup(patcher.stop)
        patcher.start()

        patcher = mock.patch("node.shutil.rmtree")
        self.addCleanup(patcher.stop)
        patcher.start()

    def test_retries_after_transient_failure_then_succeeds(self):
        with mock.patch(
            "node.urllib.request.urlretrieve",
            side_effect=[urllib.error.URLError("connection reset"), None],
        ) as urlretrieve, mock.patch(
            "node._verify_sha256"
        ), mock.patch(
            "node.tarfile.open"
        ):
            bin_dir = node.install_node("/fake/install/dir")

        self.assertEqual(urlretrieve.call_count, 2)
        self.sleep.assert_called_once_with(node._DOWNLOAD_RETRY_DELAY_SECONDS)
        self.assertIsNotNone(bin_dir)

    def test_gives_up_after_exhausting_all_attempts(self):
        with mock.patch(
            "node.urllib.request.urlretrieve",
            side_effect=urllib.error.URLError("connection reset"),
        ) as urlretrieve:
            result = node.install_node("/fake/install/dir")

        self.assertIsNone(result)
        self.assertEqual(urlretrieve.call_count, node._DOWNLOAD_ATTEMPTS)
        self.assertEqual(
            self.sleep.call_args_list,
            [mock.call(node._DOWNLOAD_RETRY_DELAY_SECONDS)]
            * (node._DOWNLOAD_ATTEMPTS - 1),
        )

    def test_does_not_retry_non_network_errors(self):
        with mock.patch(
            "node.urllib.request.urlretrieve",
            side_effect=PermissionError("permission denied"),
        ) as urlretrieve:
            result = node.install_node("/fake/install/dir")

        self.assertIsNone(result)
        self.assertEqual(urlretrieve.call_count, 1)
        self.sleep.assert_not_called()


if __name__ == "__main__":
    unittest.main()
