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

"""
Phase-driven SandboxClaim churn generator.

Drives a realistic traffic profile that ClusterLoader2 cannot express:
  - variable arrival rate across phases (e.g. 5/s baseline -> 50/s peak)
  - per-claim TTL via shutdownTime (CL2 templates have no per-object "now"
    to compute creation+TTL)

Each claim is stamped with `lifecycle.shutdownTime = now + shutdown_ttl` and
`shutdownPolicy: Retain`. The SandboxClaim controller deletes the underlying
Sandbox/Pod at shutdownTime (proactively, not via GC cascade); the lightweight
claim objects are bulk-deleted at the end. The driver issues NO per-claim
deletes during the run — it only creates.

Measurement stays in Prometheus / Cloud Monitoring via the controller's
agent_sandbox_claim_controller_startup_latency_ms and
agent_sandbox_claim_creation_total metrics. This driver only generates load and
records what it did (stats CSV for correlating population against latency/nodes).

Usage:
  python3 churn-driver.py \
    --namespace agent-sandbox-churn \
    --warmpool churn-warmpool \
    --shutdown-ttl 120 \
    --profile "5:150,30:300,5:150,0:1200" \
    --stats-file ./tmp/run-x/churn-stats.csv

Profile format: comma-separated "rate:duration_seconds" segments, executed in
order. A "0:N" segment is an idle pause (no creation) for N seconds.
"""

import argparse
import csv
import signal
import sys
import threading
import time
import heapq
import math
from concurrent.futures import ThreadPoolExecutor

try:
    from kubernetes import client, config
except ImportError:
    client = config = None

GROUP = "extensions.agents.x-k8s.io"
VERSION = "v1beta1"
PLURAL = "sandboxclaims"
RUN_LABEL_KEY = "load-test-run"
# Annotation the claim controller stamps on a claim when it adopts a warm
# sandbox. Its presence == "this claim adopted (didn't expire before adopting)".
ADOPTED_ANNOTATION = "agents.x-k8s.io/sandbox-name"
# How often to prune the in-memory live set so the population stat tracks the
# controller's shutdownTime expiry. Bookkeeping only — no API calls.
BOOKKEEPING_INTERVAL_S = 5


class ChurnDriver:
    def __init__(self, args):
        self.ns = args.namespace
        self.warmpool = args.warmpool
        self.shutdown_ttl = args.shutdown_ttl
        self.run_id = args.run_id
        self.stats_file = args.stats_file
        self.phases = self._parse_profile(args.profile)

        if config is None:
            sys.exit("kubernetes python client not installed: pip3 install kubernetes")
        try:
            config.load_kube_config()
        except Exception:
            config.load_incluster_config()
        self.api = client.CustomObjectsApi()

        # Oldest-first set of (name, created_at) for the population stat only.
        self.live = []
        self.live_lock = threading.Lock()
        self.created_total = 0
        self.create_errors = 0
        self.counter = 0
        self.current_phase = "init"
        self.current_rate = 0.0
        self.stop = threading.Event()
        self.in_flight = threading.BoundedSemaphore(128)

        # 50/s+ with ~50-100ms API latency needs real concurrency for creation.
        self.create_pool = ThreadPoolExecutor(max_workers=64)

    @staticmethod
    def _parse_profile(profile):
        phases = []
        if not profile.strip():
            raise ValueError("empty profile")
        for seg in profile.split(","):
            try:
                rate, dur = seg.strip().split(":")
                rate = float(rate)
                dur = int(dur)
            except ValueError as exc:
                raise ValueError(
                    f"invalid profile segment {seg!r}; expected rate:duration_seconds"
                ) from exc
            if not math.isfinite(rate) or rate < 0:
                raise ValueError(
                    f"invalid profile segment {seg!r}; rate must be a finite number >= 0"
                )
            if dur <= 0:
                raise ValueError(f"invalid profile segment {seg!r}; duration must be > 0")
            phases.append((rate, dur))
        if not phases:
            raise ValueError("empty profile")
        return phases

    # --- create path ---

    def _create_one(self):
        self.in_flight.acquire()
        self.counter += 1
        name = f"churn-{self.run_id}-{self.counter}"
        # shutdownTime is an absolute RFC3339 time = now + TTL. The controller
        # deletes the Sandbox/Pod at that moment (Retain keeps the claim object,
        # bulk-cleaned at drain). This is why a driver is needed: CL2 cannot
        # compute a per-claim creation+TTL timestamp.
        expire_at = time.time() + self.shutdown_ttl
        expire = time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime(expire_at))
        body = {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "SandboxClaim",
            "metadata": {
                "name": name,
                "namespace": self.ns,
                "labels": {RUN_LABEL_KEY: self.run_id},
            },
            "spec": {
                "warmPoolRef": {"name": self.warmpool},
                "lifecycle": {"shutdownTime": expire, "shutdownPolicy": "Retain"},
            },
        }

        def do():
            try:
                self.api.create_namespaced_custom_object(
                    group=GROUP, version=VERSION, namespace=self.ns,
                    plural=PLURAL, body=body)
                with self.live_lock:
                    heapq.heappush(self.live, expire_at)
                    self.created_total += 1
            except Exception as e:
                with self.live_lock:
                    self.create_errors += 1
                print(f"[create] ERROR {name}: {e}", file=sys.stderr)
            finally:
                self.in_flight.release()

        try:
            self.create_pool.submit(do)
        except Exception:
            self.in_flight.release()
            raise

    # --- population bookkeeping (no deletes; controller handles expiry) ---

    def _bookkeeping_loop(self):
        """Prune the live set as claims pass their shutdownTime, so the stats
        `population` column reflects the live count (~rate x ttl). Issues no
        API calls — the controller does the actual deletion."""
        while not self.stop.is_set():
            now = time.time()
            with self.live_lock:
                while self.live and self.live[0] <= now:
                    heapq.heappop(self.live)
            self.stop.wait(BOOKKEEPING_INTERVAL_S)

    def _population(self):
        with self.live_lock:
            return len(self.live)

    # --- stats ---

    def _stats_loop(self):
        if not self.stats_file:
            while not self.stop.is_set():
                row = [int(time.time()), self.current_phase, self.current_rate,
                       self._population(), self.created_total, self.create_errors]
                print(f"[stats] phase={row[1]} rate={row[2]}/s pop={row[3]} "
                      f"created={row[4]} errs={row[5]}")
                self.stop.wait(10)
            return

        try:
            with open(self.stats_file, "w", newline="") as f:
                writer = csv.writer(f)
                writer.writerow(["unix_ts", "phase", "target_rate", "population",
                                 "created_total", "create_errors"])
                while not self.stop.is_set():
                    row = [int(time.time()), self.current_phase, self.current_rate,
                           self._population(), self.created_total, self.create_errors]
                    print(f"[stats] phase={row[1]} rate={row[2]}/s pop={row[3]} "
                          f"created={row[4]} errs={row[5]}")
                    writer.writerow(row)
                    f.flush()
                    self.stop.wait(10)
        except Exception as e:
            print(f"[stats] file error: {e}", file=sys.stderr)

    # --- main loop ---

    def _run_phase(self, idx, rate, duration):
        self.current_phase = f"{idx + 1}/{len(self.phases)}"
        self.current_rate = rate
        print(f"=== Phase {self.current_phase}: {rate}/s for {duration}s "
              f"(pop now {self._population()}) ===")
        if rate <= 0:  # idle / pause phase
            self.stop.wait(duration)
            return
        end = time.time() + duration
        interval = 1.0 / rate
        next_t = time.time()
        # Drift-corrected scheduler: at high rates every ms matters.
        while time.time() < end and not self.stop.is_set():
            self._create_one()
            next_t += interval
            delay = next_t - time.time()
            if delay > 0:
                time.sleep(delay)
            else:
                next_t = time.time()  # fell behind; don't burst to catch up

    def _survivorship_report(self):
        """Count claims that adopted vs expired before adopting.

        The latency metric only records claims that became Ready, so claims that
        expired before adopting are invisible to it. A non-zero count means
        adoption is slower than shutdownTime — at/over the sustainable rate, or
        shutdown-ttl too short.
        """
        total = adopted = expired = pending = 0
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        cont = None
        try:
            while True:
                kwargs = dict(group=GROUP, version=VERSION, namespace=self.ns,
                              plural=PLURAL,
                              label_selector=f"{RUN_LABEL_KEY}={self.run_id}",
                              limit=500)
                if cont:
                    kwargs["_continue"] = cont
                resp = self.api.list_namespaced_custom_object(**kwargs)
                for c in resp.get("items", []):
                    total += 1
                    ann = c.get("metadata", {}).get("annotations") or {}
                    if ann.get(ADOPTED_ANNOTATION):
                        adopted += 1
                        continue
                    shutdown = (
                        c.get("spec", {})
                        .get("lifecycle", {})
                        .get("shutdownTime")
                    )
                    if shutdown and shutdown <= now:
                        expired += 1
                    else:
                        pending += 1
                cont = resp.get("metadata", {}).get("continue")
                if not cont:
                    break
        except Exception as e:
            print(f"Survivorship report failed: {e}", file=sys.stderr)
            return

        never = expired
        pct = (100.0 * never / total) if total else 0.0
        print("=== Survivorship (claim objects at drain) ===")
        print(f"  driver_created={self.created_total} claims={total} "
              f"adopted={adopted} expired_before_adopt={never} ({pct:.2f}%)")
        if pending:
            print(f"  note: {pending} claims were still live at drain and were "
                  "excluded from expired_before_adopt.")
        if never > 0:
            print(f"  WARNING: {never} claims expired before adopting. Adoption is "
                  f"slower than shutdownTime ({self.shutdown_ttl}s) — near/over the "
                  f"sustainable rate, or shutdown-ttl too short. Latency metric is "
                  f"survivorship-biased.")
        else:
            print("  All claims adopted before expiry — latency metric is NOT "
                  "survivorship-biased. ✓")

    def _drain(self):
        """End-of-run cleanup: report survivorship, then bulk-delete all claim
        objects for this run (Retain'd expired ones + any still-active recent
        ones, whose sandboxes GC via ownerRef)."""
        self.current_phase = "drain"
        self.current_rate = 0.0
        self._survivorship_report()
        try:
            self.api.delete_collection_namespaced_custom_object(
                group=GROUP, version=VERSION, namespace=self.ns, plural=PLURAL,
                label_selector=f"{RUN_LABEL_KEY}={self.run_id}")
            print("Bulk-deleted all claim objects for this run.")
        except Exception as e:
            print(f"Bulk claim cleanup failed (clean up manually with "
                  f"`kubectl delete sandboxclaims -n {self.ns} "
                  f"-l {RUN_LABEL_KEY}={self.run_id}`): {e}", file=sys.stderr)
        print(f"Drain complete. created={self.created_total} "
              f"create_errors={self.create_errors}")

    def run(self):
        threading.Thread(target=self._stats_loop, daemon=True).start()
        threading.Thread(target=self._bookkeeping_loop, daemon=True).start()

        total = sum(d for _, d in self.phases)
        print(f"Profile: {self.phases} — total {total}s (~{total // 60}m), "
              f"shutdown_ttl={self.shutdown_ttl}s, run-id={self.run_id}")
        for idx, (rate, duration) in enumerate(self.phases):
            if self.stop.is_set():
                break
            self._run_phase(idx, rate, duration)

        self.create_pool.shutdown(wait=True)
        self._drain()
        self.stop.set()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--namespace", required=True)
    p.add_argument("--warmpool", required=True)
    p.add_argument("--profile", required=True,
                   help='comma-separated "rate:duration_seconds" segments')
    p.add_argument("--shutdown-ttl", type=int, default=120,
                   help="per-claim lifetime: shutdownTime = now + N is stamped on "
                        "each claim; the controller deletes the sandbox at that "
                        "time (shutdownPolicy Retain). population ~= rate x N.")
    p.add_argument("--run-id", default=str(int(time.time())))
    p.add_argument("--stats-file", default="")
    args = p.parse_args()
    if args.shutdown_ttl <= 0:
        p.error("--shutdown-ttl must be > 0")

    driver = ChurnDriver(args)

    def on_sigint(_sig, _frm):
        print("\nSIGINT — stopping creation, cleaning up...")
        driver.stop.set()

    signal.signal(signal.SIGINT, on_sigint)
    driver.run()


if __name__ == "__main__":
    main()
