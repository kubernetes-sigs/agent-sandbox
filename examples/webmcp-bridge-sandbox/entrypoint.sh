#!/bin/sh
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
set -e

# Serves demo-page/ so bridge.py has something to point Playwright at.
# A real deployment would point TARGET_PAGE_URL at whatever page actually
# registers WebMCP tools instead of running one in the same pod.
python3 -m http.server 8090 --directory demo-page &
HTTP_PID=$!

# Nothing supervises this after bridge.py starts — if it dies later,
# bridge.py's calls just start failing with no obvious tie-back to "the
# demo page server is gone." Log clearly if that happens, so a failure
# here doesn't look like an unrelated bridge.py problem.
#
# This subshell outlives the `exec` below, at which point its parent
# (this script's own shell process) has been replaced by bridge.py and
# it becomes an orphan reparented to PID 1. That's fine here: it doesn't
# hold or need stdin, and its stdout/stderr file descriptors are
# inherited, not closed, by exec — so its warning still reaches the
# container's log stream if it ever fires, orphaned or not.
( while kill -0 "$HTTP_PID" 2>/dev/null; do sleep 1; done
  echo "demo page http.server (pid $HTTP_PID) exited unexpectedly" >&2
) &

# bridge.py's page.goto() has no retry of its own, so wait for the server
# to actually be accepting connections before handing off — without this,
# a slower runner can start bridge.py before http.server has bound the
# port, and the navigation fails instead of racing to a working state.
python3 -c "
import socket, time
for _ in range(50):
    try:
        socket.create_connection(('localhost', 8090), timeout=0.2).close()
        break
    except OSError:
        time.sleep(0.1)
else:
    raise SystemExit('demo page server never came up on :8090')
"

exec python3 bridge.py
