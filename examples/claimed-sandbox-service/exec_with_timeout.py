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

"""Bound a verifier exec stream and terminate its owned local process group.

kubectl's request timeout does not cover the lifetime of an upgraded exec stream.
Python's standard library supplies a portable process deadline on macOS/Linux.
"""

import os
import signal
import subprocess
import sys


def terminate(signum, _frame):
    raise SystemExit(128 + signum)


def main():
    signal.signal(signal.SIGTERM, terminate)
    # Resolve the command as Bash does in verify.sh, including exported functions.
    # Pass arguments positionally rather than interpolating them into shell code.
    process = subprocess.Popen(
        ["bash", "-c", '"$@"', "kubectl-exec", *sys.argv[1:]],
        stdin=subprocess.DEVNULL, start_new_session=True,
    )
    try:
        try:
            status = process.wait(timeout=15)
            return status if status >= 0 else 128 - status
        except subprocess.TimeoutExpired:
            print("kubectl exec exceeded its 15-second process deadline.", file=sys.stderr)
            return 124
    finally:
        # Kill the entire owned group: a descendant can otherwise keep stdout open.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


if __name__ == "__main__":
    sys.exit(main())
