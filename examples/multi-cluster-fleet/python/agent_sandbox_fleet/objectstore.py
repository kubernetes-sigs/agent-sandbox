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

"""Object-store client.

Thin GCS wrapper for the planner + trainer. Matches the schemas the Go agent
reads/writes in `pkg/objectstore/`. Kept intentionally minimal — Python side
only needs Get/Put/List; watch/etag is implemented in the Go agent.

Prefix layout (see ARCHITECTURE.md):
  fleet/spec.json
  fleet/assignments.json
  fleet/capacity/<cluster>.json
  weights/manifest.json
  weights/deltas/v<N>.bin
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from typing import Any

# NOTE: `google.cloud.storage` is imported LAZILY inside GCS.__init__ so that
# consumers who only need `Paths` (e.g. planner.py's `Paths()` default arg,
# or pure-unit tests) can `import agent_sandbox_fleet.objectstore` without
# having google-cloud-storage installed.


class CASConflict(RuntimeError):
    """A conditional write lost: the object changed since it was read.

    Raised by `put_json(..., if_generation_match=N)` when the store's own
    generation for the object is no longer N. The caller must re-read and
    re-derive rather than retrying the same bytes -- the point of the
    precondition is that the state it was computed from is gone.
    """


@dataclass
class Paths:
    """Canonical object paths — do NOT hardcode elsewhere."""
    spec: str = "fleet/spec.json"
    assignments: str = "fleet/assignments.json"
    capacity_prefix: str = "fleet/capacity/"
    weight_manifest: str = "weights/manifest.json"
    weight_delta_prefix: str = "weights/deltas/"

    def capacity(self, cluster: str) -> str:
        return f"{self.capacity_prefix}{cluster}.json"

    def weight_delta(self, version: int) -> str:
        return f"{self.weight_delta_prefix}v{version}.bin"


@dataclass
class BucketRef:
    bucket: str
    paths: Paths = field(default_factory=Paths)


class GCS:
    """Minimal GCS façade — reads/writes JSON + bytes, lists a prefix."""

    def __init__(self, bucket: str):
        # Lazy import so the module is loadable without google-cloud-storage;
        # only actual GCS use requires the dep.
        from google.api_core import exceptions as gexc
        from google.cloud import storage
        # Bound here rather than imported at module scope so the lazy-import
        # contract above still holds for Paths-only consumers.
        self._gexc = gexc
        self._client = storage.Client()
        self._bucket_name = bucket
        self._bucket = self._client.bucket(bucket)

    @property
    def bucket_name(self) -> str:
        return self._bucket_name

    def put_json(self, path: str, obj: Any,
                 if_generation_match: int | None = None) -> None:
        """Write JSON, optionally only if the object is still at a known revision.

        `if_generation_match` is the STORE's generation for the object, not the
        `generation` field inside the payload -- the two are unrelated counters
        and only the store's is a safe concurrency token, because only the store
        increments it on every write. Pass 0 to mean "this object must not exist
        yet", which is what makes the first write of a fleet safe without a
        separate create path. Omit it entirely for last-writer-wins, which is
        correct for capacity reports: each is written by exactly one member and
        a lost update is superseded 30 seconds later anyway.

        Raises CASConflict when the precondition fails. `S3-compatible stores
        spell the same thing `If-Match` on the ETag; the caller-facing contract
        here is deliberately store-agnostic so a future S3 backend can raise the
        same exception.
        """
        blob = self._bucket.blob(path)
        try:
            blob.upload_from_string(
                json.dumps(obj, indent=2, sort_keys=True).encode("utf-8"),
                content_type="application/json",
                if_generation_match=if_generation_match,
            )
        except self._gexc.PreconditionFailed as e:
            raise CASConflict(
                f"{path} changed since it was read (expected store generation "
                f"{if_generation_match}); re-read and re-plan"
            ) from e

    def get_json_with_generation(self, path: str) -> tuple[Any | None, int]:
        """Return (parsed JSON or None, store generation).

        The generation is 0 when the object is absent, which is exactly the
        value `put_json` wants for a "must not exist" precondition -- so a
        read-modify-write can be written once and work on both the first apply
        and every later one, with no `if absent` branch to get wrong.
        """
        # Same two-request race as get_with_etag: an overwrite landing between
        # get_blob() and the pinned download fails the precondition, and there
        # is then a newer generation to fetch. Bounded for the same reason —
        # a writer outrunning the reader should surface, not spin.
        for _ in range(3):
            blob = self._bucket.get_blob(path)
            if blob is None:
                return None, 0
            try:
                raw = blob.download_as_bytes(if_generation_match=blob.generation)
            except self._gexc.NotFound:
                return None, 0
            except self._gexc.PreconditionFailed:
                continue
            return json.loads(raw.decode("utf-8")), blob.generation
        raise RuntimeError(
            f"{path} changed generation on every read attempt; the writer is "
            "outrunning the reader"
        )

    def get_json(self, path: str) -> Any | None:
        # One request, not exists()-then-download: the two calls can observe
        # different object versions, and the miss path costs an extra round trip
        # on every poll.
        blob = self._bucket.blob(path)
        try:
            raw = blob.download_as_bytes()
        except self._gexc.NotFound:
            return None
        return json.loads(raw.decode("utf-8"))

    def put_bytes(self, path: str, data: bytes, content_type: str = "application/octet-stream") -> None:
        blob = self._bucket.blob(path)
        blob.upload_from_string(data, content_type=content_type)

    def list_prefix(self, prefix: str) -> list[str]:
        return [b.name for b in self._client.list_blobs(self._bucket_name, prefix=prefix)]

    def object_age_s(self, path: str) -> float | None:
        blob = self._bucket.get_blob(path)
        if blob is None or blob.updated is None:
            return None
        return time.time() - blob.updated.timestamp()

    def get_with_etag(self, path: str) -> tuple[bytes, str]:
        """Return (raw_bytes, generation_etag). Raises FileNotFoundError if absent.

        The fleet-member's reconciler uses this to skip re-processing an unchanged
        assignments.json (compares the returned etag to the one it stored last).
        GCS generation numbers are used as opaque etag strings. The download is
        pinned with `if_generation_match` so the bytes and the etag always come
        from the same generation: get_blob() and download_as_bytes() are two
        requests, and a write landing between them would otherwise hand the
        caller new bytes tagged with the old generation (or vice versa). The
        fleet-member caches that etag, so a mismatched pair means it sees an
        unchanged etag on the next tick and never re-reads the update — a cluster
        left serving a superseded plan indefinitely.
        """
        # Re-read on 412: the object was rewritten between the metadata request
        # and the download, so there is a newer generation to fetch. Bounded,
        # because a bucket being written faster than we can read it should
        # surface as an error rather than spin.
        for _ in range(3):
            blob = self._bucket.get_blob(path)
            if blob is None:
                raise FileNotFoundError(path)
            try:
                data = blob.download_as_bytes(if_generation_match=blob.generation)
            except self._gexc.NotFound:
                raise FileNotFoundError(path) from None
            except self._gexc.PreconditionFailed:
                continue
            return data, f"gen:{blob.generation}"
        raise RuntimeError(
            f"{path} changed generation on every read attempt; the writer is "
            "outrunning the reader"
        )
