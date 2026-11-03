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

"""GPU state probe for demonstrating GKE Pod Snapshot restores on an L4.

Holds state where only a GPU-aware snapshot can capture it:
  - process memory: a boot nonce, generated once per cold process start
  - GPU memory: a BUFFER_MIB float32 buffer seeded from the nonce, and a
    counter. POST /bump increments both on the GPU, so after a bump the
    current values exist only in device memory.

A restored process reports the same nonce, counter, and buffer checksum it
had at snapshot time. A cold-started process reports a new nonce, a zero
counter, and a different checksum.

GET  /healthz -> {"ok": true}
GET  /state   -> {"boot_nonce", "counter", "checksum", "gpu", "buffer_mib"}
POST /bump    -> adds 1 to the GPU counter and to each buffer element
"""

import json
import os
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

import cupy as cp
import numpy as np

BOOT_NONCE = uuid.uuid4().hex
BUFFER_MIB = int(os.environ.get("BUFFER_MIB", "256"))

# Seed on the host, then keep the only copy on the GPU.
_host = np.random.default_rng(int(BOOT_NONCE[:8], 16)).random(
    BUFFER_MIB * 1024 * 1024 // 4, dtype=np.float32)
BUFFER = cp.asarray(_host)
del _host
COUNTER = cp.zeros(1, dtype=cp.int64)
GPU_NAME = cp.cuda.runtime.getDeviceProperties(0)["name"].decode()


def state():
    return {
        "boot_nonce": BOOT_NONCE,
        "counter": int(COUNTER.get()[0]),
        # float64 accumulation keeps the sum stable across calls.
        "checksum": repr(float(BUFFER.sum(dtype=cp.float64))),
        "gpu": GPU_NAME,
        "buffer_mib": BUFFER_MIB,
    }


class Handler(BaseHTTPRequestHandler):
    def _reply(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            return self._reply(200, {"ok": True})
        if self.path == "/state":
            return self._reply(200, state())
        self._reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path == "/bump":
            COUNTER[0] += 1
            BUFFER[...] += 1.0
            return self._reply(200, state())
        self._reply(404, {"error": "not found"})

    def log_message(self, *args):  # keep container logs quiet
        pass


if __name__ == "__main__":
    print(f"gpu-probe ready on {GPU_NAME}, nonce {BOOT_NONCE}", flush=True)
    HTTPServer(("0.0.0.0", 8888), Handler).serve_forever()
