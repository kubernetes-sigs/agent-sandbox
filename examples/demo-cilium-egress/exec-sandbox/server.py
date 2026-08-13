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

# Minimal HTTP "exec" server for the ingress demo (Python stdlib only).
#
# ⚠️ DEMO ONLY: /execute runs caller-supplied commands (arbitrary code execution
# by design — it's how the demo drives `git clone` to exercise the egress
# policy). Do NOT expose this on a shared/long-lived cluster. The optional
# ingress add-on fronts it with a public Gateway only for the throwaway demo;
# for real use run the router authenticated (ROUTER_AUTH_TOKEN) / keep it private.
# Endpoints:
#   GET  /healthz  -> {"status":"ok"}   (used by the GKE Gateway health check path too)
#   POST /execute  -> body {"command":"..."} runs the command and returns
#                     {"stdout","stderr","exit_code"}
# The sandbox-router proxies requests here (default sandbox port 8888). The command
# runs inside the sandbox pod, so its egress is governed by the CiliumNetworkPolicy
# bound to the pod's identity — that's what makes `git clone` succeed or fail.
import json
import shlex
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8888


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path in ("/healthz", "/"):
            self._send(200, {"status": "ok"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/execute":
            self._send(404, {"error": "not found"})
            return
        n = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            cmd = json.loads(raw or b"{}").get("command", "")
        except Exception:
            self._send(400, {"error": "invalid json"})
            return
        if not cmd:
            self._send(400, {"error": "missing command"})
            return
        try:
            argv = shlex.split(cmd)
        except ValueError as err:
            self._send(400, {"error": "invalid command syntax: %s" % err})
            return
        try:
            p = subprocess.run(argv, capture_output=True, text=True, timeout=120)
            self._send(200, {"stdout": p.stdout, "stderr": p.stderr, "exit_code": p.returncode})
        except subprocess.TimeoutExpired:
            self._send(200, {"stdout": "", "stderr": "command timed out", "exit_code": 124})
        except (FileNotFoundError, OSError) as err:
            self._send(200, {"stdout": "", "stderr": "exec failed: %s" % err, "exit_code": 127})

    def log_message(self, *args):  # quiet
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
