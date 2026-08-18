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

"""GCS read-path tests.

GCS.__init__ constructs a live storage.Client(), so these build the façade
with object.__new__ and inject a fake bucket plus a fake exceptions module --
the same shape google.api_core.exceptions has, which is all the code touches.
"""

from __future__ import annotations

import json
import types

import pytest

from agent_sandbox_fleet.objectstore import GCS


class _Exc:
    """Stand-in for google.api_core.exceptions."""

    class NotFound(Exception):
        pass

    class PreconditionFailed(Exception):
        pass


class _Blob:
    def __init__(self, bucket, path):
        self._bucket = bucket
        self._path = path

    @property
    def generation(self):
        return self._bucket.generations.get(self._path)

    def download_as_bytes(self, if_generation_match=None):
        self._bucket.downloads.append((self._path, if_generation_match))
        # Let the test advance the object between the metadata read and this one.
        hook = self._bucket.on_download
        if hook is not None:
            hook()
        if self._path not in self._bucket.data:
            raise _Exc.NotFound(self._path)
        if (if_generation_match is not None
            and if_generation_match != self._bucket.generations[self._path]):
            raise _Exc.PreconditionFailed(self._path)
        return self._bucket.data[self._path]


class _Bucket:
    def __init__(self):
        self.data: dict[str, bytes] = {}
        self.generations: dict[str, int] = {}
        self.downloads: list[tuple[str, int | None]] = []
        self.exists_calls = 0
        self.on_download = None

    def put(self, path, raw, generation=1):
        self.data[path] = raw
        self.generations[path] = generation

    def blob(self, path):
        return _Blob(self, path)

    def get_blob(self, path):
        return _Blob(self, path) if path in self.data else None


def _gcs():
    g = object.__new__(GCS)
    g._bucket = _Bucket()
    g._bucket_name = "fake"
    g._gexc = _Exc
    return g


# --------------------------------------------------------------------------- #
# get_json
# --------------------------------------------------------------------------- #

def test_get_json_reads_the_object():
    g = _gcs()
    g._bucket.put("a.json", b'{"x": 1}')
    assert g.get_json("a.json") == {"x": 1}


def test_get_json_costs_one_request():
    # It used to call blob.exists() and then download_as_bytes(). Two round
    # trips per read, on a path the planner walks once per capacity report per
    # tick -- and the two can observe different object versions.
    g = _gcs()
    g._bucket.put("a.json", b'{"x": 1}')
    g.get_json("a.json")
    assert len(g._bucket.downloads) == 1
    assert g._bucket.exists_calls == 0


def test_get_json_returns_none_for_a_missing_object():
    assert _gcs().get_json("nope.json") is None


# --------------------------------------------------------------------------- #
# get_with_etag — the fleet-member's change detector
# --------------------------------------------------------------------------- #

def test_get_with_etag_pins_the_download_to_the_generation_it_reports():
    g = _gcs()
    g._bucket.put("assignments.json", b'{"generation": 1}', generation=11)
    data, etag = g.get_with_etag("assignments.json")
    assert data == b'{"generation": 1}'
    assert etag == "gen:11"
    assert g._bucket.downloads == [("assignments.json", 11)], (
        "the download was not generation-matched; bytes and etag can come from "
        "different generations, and the member caches that etag"
    )


def test_a_write_landing_mid_read_is_re_read_not_mis_tagged():
    # THE BUG. get_blob() and download_as_bytes() are two requests. A publish
    # landing between them handed back new bytes tagged with the old generation
    # (or the reverse). The member stores that etag, sees it again next tick,
    # concludes "unchanged" and never re-reads -- a cluster serving a superseded
    # plan for as long as the plan holds steady.
    g = _gcs()
    g._bucket.put("assignments.json", b'{"generation": 1}', generation=11)

    def publish_once():
        g._bucket.on_download = None
        g._bucket.put("assignments.json", b'{"generation": 2}', generation=12)

    g._bucket.on_download = publish_once

    data, etag = g.get_with_etag("assignments.json")
    assert json.loads(data)["generation"] == 2
    assert etag == "gen:12", "etag describes a generation the bytes did not"


def test_get_with_etag_raises_file_not_found_when_absent():
    with pytest.raises(FileNotFoundError):
        _gcs().get_with_etag("nope.json")


def test_a_delete_between_the_two_requests_is_a_file_not_found():
    g = _gcs()
    g._bucket.put("assignments.json", b"{}", generation=1)

    def delete_it():
        g._bucket.data.pop("assignments.json")

    g._bucket.on_download = delete_it
    with pytest.raises(FileNotFoundError):
        g.get_with_etag("assignments.json")


def test_a_writer_that_outruns_the_reader_errors_rather_than_spinning():
    # Bounded retry. An object rewritten on every single attempt is a real
    # problem worth surfacing, not something to loop on forever inside a
    # reconcile tick.
    g = _gcs()
    g._bucket.put("assignments.json", b"{}", generation=1)
    counter = {"n": 1}

    def always_bump():
        counter["n"] += 1
        g._bucket.generations["assignments.json"] = counter["n"]

    g._bucket.on_download = always_bump
    with pytest.raises(RuntimeError, match="outrunning"):
        g.get_with_etag("assignments.json")
    assert len(g._bucket.downloads) == 3, "retry is not bounded at 3"
