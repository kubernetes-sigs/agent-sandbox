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

"""Tiny state probe for demonstrating GKE Pod Snapshot restores.

Holds state in two places a snapshot must capture:
  - process memory: a boot nonce (random, generated once per cold process
    start) and a counter mutated via /bump — a restored process keeps both,
    a fresh process gets a new nonce and a zero counter
  - container rootfs: /state/marker.txt written via /write — not a volume,
    so it only survives into a new pod via snapshot restore

GET  /state  -> {"boot_nonce": ..., "counter": N, "marker": <file or null>}
POST /bump   -> increments the in-memory counter
POST /write  -> writes the request body to /state/marker.txt
"""

import json
import os
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

BOOT_NONCE = uuid.uuid4().hex
COUNTER = 0
MARKER_PATH = "/state/marker.txt"


class Handler(BaseHTTPRequestHandler):
    def _reply(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path != "/state":
            return self._reply(404, {"error": "not found"})
        marker = None
        if os.path.exists(MARKER_PATH):
            with open(MARKER_PATH) as f:
                marker = f.read()
        self._reply(200, {"boot_nonce": BOOT_NONCE, "counter": COUNTER, "marker": marker})

    def do_POST(self):
        global COUNTER
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode() if length else ""
        if self.path == "/bump":
            COUNTER += 1
            return self._reply(200, {"counter": COUNTER})
        if self.path == "/write":
            os.makedirs(os.path.dirname(MARKER_PATH), exist_ok=True)
            with open(MARKER_PATH, "w") as f:
                f.write(body)
            return self._reply(200, {"marker": body})
        self._reply(404, {"error": "not found"})

    def log_message(self, *args):  # keep container logs quiet
        pass


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 8888), Handler).serve_forever()
