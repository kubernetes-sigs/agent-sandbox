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

import json
import subprocess
import threading
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer
from unittest import mock

from server import Handler


class ServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Bind to an OS-assigned ephemeral port so tests don't collide with a
        # real sandbox instance (or each other) on the fixed PORT=8888.
        cls.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.port = cls.httpd.server_address[1]
        cls.thread = threading.Thread(target=cls.httpd.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()
        cls.httpd.server_close()
        cls.thread.join()

    def _url(self, path):
        return f"http://127.0.0.1:{self.port}{path}"

    def _post(self, path, data):
        req = urllib.request.Request(self._url(path), data=data, method="POST")
        try:
            resp = urllib.request.urlopen(req)
            return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_healthz(self):
        resp = urllib.request.urlopen(self._url("/healthz"))
        self.assertEqual(resp.status, 200)
        self.assertEqual(json.loads(resp.read()), {"status": "ok"})

    def test_root_also_reports_healthy(self):
        resp = urllib.request.urlopen(self._url("/"))
        self.assertEqual(json.loads(resp.read()), {"status": "ok"})

    def test_unknown_get_path_returns_404(self):
        with self.assertRaises(urllib.error.HTTPError) as cm:
            urllib.request.urlopen(self._url("/nope"))
        self.assertEqual(cm.exception.code, 404)

    def test_execute_runs_command_and_captures_output(self):
        status, body = self._post("/execute", json.dumps({"command": "echo hello"}).encode())
        self.assertEqual(status, 200)
        self.assertEqual(body["stdout"], "hello\n")
        self.assertEqual(body["stderr"], "")
        self.assertEqual(body["exit_code"], 0)

    def test_execute_missing_command_returns_400(self):
        status, body = self._post("/execute", json.dumps({}).encode())
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_execute_invalid_json_returns_400(self):
        status, body = self._post("/execute", b"not json")
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_execute_unparsable_shell_syntax_returns_400(self):
        status, body = self._post("/execute", json.dumps({"command": 'echo "unterminated'}).encode())
        self.assertEqual(status, 400)
        self.assertIn("error", body)

    def test_execute_nonexistent_binary_returns_127_not_500(self):
        status, body = self._post("/execute", json.dumps({"command": "this-binary-does-not-exist-xyz"}).encode())
        self.assertEqual(status, 200)
        self.assertEqual(body["exit_code"], 127)

    @mock.patch("server.subprocess.run")
    def test_execute_timeout_returns_124(self, mock_run):
        mock_run.side_effect = subprocess.TimeoutExpired(cmd="sleep 1000", timeout=120)
        status, body = self._post("/execute", json.dumps({"command": "sleep 1000"}).encode())
        self.assertEqual(status, 200)
        self.assertEqual(body["exit_code"], 124)

    def test_unknown_post_path_returns_404(self):
        status, body = self._post("/notexecute", json.dumps({"command": "echo hi"}).encode())
        self.assertEqual(status, 404)


if __name__ == "__main__":
    unittest.main()
