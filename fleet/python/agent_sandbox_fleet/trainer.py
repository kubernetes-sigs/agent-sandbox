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

"""Mock trainer — writes fake weight deltas + manifest to GCS.

Models the Cognition SWE-1.7 pattern (July 2026):
  every K gradient steps → compressed delta → upload → publish manifest

The delta is gzip'd random bytes, sized to `--delta-size` (default 1 MiB).
Real production would produce a tensor SVD or top-K sparsified delta and
include a checksum + per-shard layout. The wire schema (`WeightManifest`) is
identical either way, so the agent code doesn't change.

Run standalone:
  python -m agent_sandbox_fleet.trainer --bucket $FLEET_BUCKET \\
      --steps 5 --interval 10 --delta-size 1MiB
"""

from __future__ import annotations

import argparse
import datetime as _dt
import gzip
import hashlib
import logging
import os
import secrets
import time

from .objectstore import GCS, Paths

logger = logging.getLogger("agent_sandbox_fleet.trainer")

# Longest suffix first: "MiB" must be tested before "B", or "1MiB" matches "B"
# and float("1Mi") raises.
_SIZES = (
    ("GiB", 1024 * 1024 * 1024),
    ("MiB", 1024 * 1024),
    ("KiB", 1024),
    ("B", 1),
)


def parse_size(s: str) -> int:
    s = s.strip()
    for suf, mult in _SIZES:
        if s.endswith(suf):
            return int(float(s[: -len(suf)]) * mult)
    return int(s)


def make_delta(size_bytes: int) -> bytes:
    """Produce a mock compressed weight delta of ~size_bytes.

    Cognition reports >99% compression via tensor decomposition; here we just
    gzip random bytes so the pipeline is realistic but the "compression" is a
    no-op. We produce `size_bytes` of random uncompressed data and gzip it.
    """
    raw = secrets.token_bytes(size_bytes)
    return gzip.compress(raw, compresslevel=1)


def publish_step(
    gcs: GCS,
    version: int,
    previous: int,
    weight_stream: str,
    size_bytes: int,
    paths: Paths | None = None,
) -> dict:
    paths = paths or Paths()
    data = make_delta(size_bytes)
    digest = hashlib.sha256(data).hexdigest()
    path = paths.weight_delta(version)
    gcs.put_bytes(path, data)
    manifest = {
        "current_version": version,
        "previous_version": previous,
        "delta_path": path,
        "delta_size_bytes": len(data),
        "delta_sha256": digest,
        "weight_stream": weight_stream,
        "published_at": _dt.datetime.now(_dt.timezone.utc).isoformat().replace("+00:00", "Z"),
    }
    gcs.put_json(paths.weight_manifest, manifest)
    logger.info("published version=%d bytes=%d sha256=%s", version, len(data), digest[:12])
    return manifest


def main() -> None:
    ap = argparse.ArgumentParser(description="Mock RL trainer — publishes weight deltas to GCS.")
    ap.add_argument("--bucket", default=os.environ.get("FLEET_BUCKET"),
                    help="GCS bucket (default: $FLEET_BUCKET)")
    ap.add_argument("--stream", default="swebench-actor-v1",
                    help="Weight stream name (must match the fleet spec)")
    ap.add_argument("--steps", type=int, default=5, help="Number of mock optimizer steps")
    ap.add_argument("--interval", type=float, default=10.0,
                    help="Seconds between step publishes")
    ap.add_argument("--delta-size", default="1MiB",
                    help="Delta payload size, e.g. 1MiB, 512KiB, 100B")
    ap.add_argument("--start-version", type=int, default=1)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(name)s %(levelname)s %(message)s",
    )
    if not args.bucket:
        ap.error("--bucket (or $FLEET_BUCKET) is required")

    size = parse_size(args.delta_size)
    gcs = GCS(args.bucket)

    prev = args.start_version - 1
    for i in range(args.steps):
        v = args.start_version + i
        publish_step(gcs, v, prev, args.stream, size)
        prev = v
        if i < args.steps - 1:
            time.sleep(args.interval)


if __name__ == "__main__":
    main()
