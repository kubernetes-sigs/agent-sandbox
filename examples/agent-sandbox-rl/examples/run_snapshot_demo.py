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

"""GKE Pod Snapshot primitives end to end: claim → snapshot → suspend → resume →
verify → restore. Asserts that both filesystem AND process memory survive.

Requires a snapshot-capable GKE cluster (Pod Snapshots enabled, gVisor runtime
class) with a PodSnapshotStorageConfig + PodSnapshotPolicy grouped by
`agents.x-k8s.io/sandbox-name-hash` whose selector matches the fleet's pod label
(`app=agent-sandbox-rl`), and a pod ServiceAccount holding the Workload Identity
bucket grants — see README → Snapshots. Env-configured:

  NAMESPACE=rl POD_SA=podsnap-sa RUNTIME_CLASS=gvisor IMAGE=python:3.12-slim \\
  python examples/run_snapshot_demo.py

The probe: a file on the container rootfs (not a volume) and a background shell
loop that increments a counter in its own memory and mirrors it to a file only on
request. A cold boot loses the file and the loop; a restore keeps both.
"""

import json
import logging
import os
import time

from agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    ResourceSpec,
    SandboxFleet,
    TemplateSpec,
)

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
log = logging.getLogger("snapshot-demo")


def _env(name, default):
  return os.getenv(name, default)


# A counter that lives in the loop's shell memory; /tmp/counter is written only
# when we `kill -USR1` the loop, so the file alone can't fake a live process.
PROBE_START = (
    "echo golden-v1 > /tmp/marker; "
    "nohup bash -c 'n=0; trap \"echo \\$n > /tmp/counter\" USR1; "
    "while sleep 0.2; do n=$((n+1)); done' >/dev/null 2>&1 & "
    "echo $! > /tmp/loop.pid; sleep 1; echo started")
PROBE_READ = (
    "kill -USR1 $(cat /tmp/loop.pid) 2>/dev/null && sleep 0.5; "
    "printf '{\"marker\":\"%s\",\"counter\":\"%s\",\"pid_alive\":%s}' "
    "\"$(cat /tmp/marker 2>/dev/null)\" \"$(cat /tmp/counter 2>/dev/null)\" "
    "$(kill -0 $(cat /tmp/loop.pid 2>/dev/null) 2>/dev/null && echo true || echo false)")


def probe(handle) -> dict:
  return json.loads(handle.exec(PROBE_READ).strip() or "{}")


def main():
  namespace = _env("NAMESPACE", "default")
  template = TemplateSpec(
      runtime_class=_env("RUNTIME_CLASS", "gvisor"),
      resources=ResourceSpec(cpu=_env("CPU", "250m"), memory=_env("MEMORY", "512Mi")),
      extra_pod_spec={"serviceAccountName": _env("POD_SA", "podsnap-sa")},
  )
  contexts = [c for c in _env("KUBE_CONTEXTS", "").split(",") if c]
  clusters = ([ClusterConfig(name=c, context=c, namespace=namespace, snapshots=True)
               for c in contexts]
              or [ClusterConfig(name="default", namespace=namespace, snapshots=True)])
  fleet = SandboxFleet(FleetConfig(
      clusters=clusters, max_concurrent=1, template=template,
      ready_timeout=int(_env("SANDBOX_READY_TIMEOUT", "600"))))
  fleet.load_tasks([_env("IMAGE", "python:3.12-slim")])

  timings = {}
  with fleet, fleet.recording("snapshot-demo") as report:
    fleet.setup()
    h = fleet.acquire(fleet.tasks[0])
    log.info("claimed %s (pod %s)", h.sandbox_id, h.pod_name)
    log.info("probe start: %s", h.exec(PROBE_START).strip())
    before = probe(h)
    assert before["marker"] == "golden-v1" and before["pid_alive"], before

    t0 = time.monotonic()
    uid = fleet.snapshot(h, "checkpoint")
    timings["snapshot_s"] = round(time.monotonic() - t0, 2)
    log.info("snapshot %s taken in %.1fs", uid, timings["snapshot_s"])
    # Let the counter advance well past the checkpoint so a restore to it is
    # distinguishable from a resume-from-latest.
    time.sleep(6)
    at_suspend = probe(h)

    t0 = time.monotonic()
    latest = fleet.suspend(h)                     # snapshots again, then deletes the pod
    timings["suspend_s"] = round(time.monotonic() - t0, 2)
    assert h.is_suspended
    old_pod = h.pod_name

    t0 = time.monotonic()
    restored = fleet.resume(h)                    # latest snapshot wins
    timings["resume_s"] = round(time.monotonic() - t0, 2)
    after = probe(h)
    log.info("resume restored=%s new pod %s (was %s): %s", restored, h.pod_name,
             old_pod, after)
    assert restored, "resume did not restore from a snapshot"
    assert h.pod_name != old_pod, "resume must bring up a new pod"
    assert after["marker"] == "golden-v1", "rootfs file lost across resume"
    assert after["pid_alive"], "background loop lost across resume (memory not restored)"
    assert int(after["counter"]) >= int(at_suspend["counter"]), (at_suspend, after)

    fleet.suspend(h, snapshot=False)
    t0 = time.monotonic()
    fleet.restore(h, uid)                         # roll back to the first checkpoint
    timings["restore_s"] = round(time.monotonic() - t0, 2)
    rolled = probe(h)
    log.info("restore(%s): %s", uid, rolled)
    assert rolled["pid_alive"] and rolled["marker"] == "golden-v1", rolled
    assert int(rolled["counter"]) < int(after["counter"]), (
        "restore to the earlier checkpoint should rewind the in-memory counter")

    fleet.release(h, delete_snapshots=_env("DELETE_SNAPSHOTS", "1") == "1")
    log.info("latest-before-suspend snapshot was %s", latest)

  print(report.summary())
  print(json.dumps({"timings": timings, "snapshots": report.to_dict()["snapshots"]},
                   indent=2))


if __name__ == "__main__":
  main()
