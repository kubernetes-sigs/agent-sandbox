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
"""Dataset-row image extraction, manifest loading and the gs:// reader."""
import subprocess
import sys
import types

import pytest

from agent_sandbox_rl import sources
from agent_sandbox_rl.sources import (
    ImageListSource, JsonlSource, Task, extract_image, load_image_list, read_text, to_tasks)


class _Arr:
  """Stands in for a numpy array: columnar datasets hand back one-element arrays."""
  def __init__(self, items):
    self._items = items
  def tolist(self):
    return list(self._items)


def test_extract_image_tries_the_r2e_and_swebench_conventions_in_order():
  assert extract_image({"image": "a", "docker_image": "b"}) == "a"
  assert extract_image({"docker_image": "b"}) == "b"
  assert extract_image({"image_name": "c"}) == "c"
  assert extract_image({"metadata": {"docker_image": "d"}}) == "d"
  assert extract_image({"metadata": {"env_config": {"entry": {"docker_image": "e"}}}}) == "e"
  assert extract_image({"env_config": {"entry": {"image": "f"}}}) == "f"
  assert extract_image({"foo": 1}) is None


def test_extract_image_unwraps_arrays_bytes_and_blank_strings():
  assert extract_image({"image": _Arr(["x:1"])}) == "x:1"
  assert extract_image({"image": [["y:1"]]}) == "y:1"
  assert extract_image({"image": b"z:1"}) == "z:1"
  assert extract_image({"image": "  "}) is None and extract_image({"image": []}) is None
  assert extract_image({"image": 42}) is None


def test_extract_image_explicit_field_sequence_and_callable():
  row = {"image": "default", "meta": {"img": "explicit"}}
  assert extract_image(row, "meta.img") == "explicit"
  assert extract_image(row, ("nope", "meta.img")) == "explicit"
  assert extract_image(row, lambda r: r["meta"]["img"].upper()) == "EXPLICIT"
  assert extract_image(row, "missing") is None


def test_to_tasks_rows_use_conventions_and_keep_the_rest_of_the_row_as_metadata():
  rows = [{"instance_id": "django-1", "docker_image": "img:1", "repo": "django"},
          {"id": "x", "image": "img:2"},
          {"metadata": {"docker_image": "img:3"}, "task_id": 7}]
  tasks = to_tasks(rows)
  assert [(t.id, t.image) for t in tasks] == [("django-1", "img:1"), ("x", "img:2"), ("7", "img:3")]
  assert tasks[0].metadata == {"repo": "django"}        # minus the keys id/image came from
  assert tasks[1].metadata == {}
  assert tasks[2].metadata == {"metadata": {"docker_image": "img:3"}}   # nested: kept whole
  assert to_tasks(rows[:1], image_field="docker_image", id_field="instance_id")[0].id == "django-1"
  with pytest.raises(KeyError, match="no container image"):
    to_tasks([{"repo": "x"}])
  with pytest.raises(KeyError, match="tried: meta.img"):
    to_tasks([{"image": "a"}], image_field="meta.img")
  # strings, Tasks and the default row index id still work
  assert [t.id for t in to_tasks(["i:1", Task(id="t", image="i:2"), {"image": "i:3"}])] == ["0", "t", "2"]


def test_jsonl_source_accepts_the_same_field_options(tmp_path):
  p = tmp_path / "tasks.jsonl"
  p.write_text('{"instance_id": "a", "docker_image": "img:a"}\n\n{"id": "b", "image": "img:b"}\n')
  tasks = JsonlSource(str(p)).load()
  assert [(t.id, t.image) for t in tasks] == [("a", "img:a"), ("b", "img:b")]
  assert JsonlSource(str(p), limit=1).load()[0].id == "a"
  assert JsonlSource(str(p), limit=0).load() == []
  bad = tmp_path / "bad.jsonl"
  bad.write_text('{"x": 1}\n')
  with pytest.raises(KeyError, match="bad.jsonl:1"):
    JsonlSource(str(bad)).load()


def test_load_image_list_skips_blanks_comments_and_duplicates(tmp_path):
  p = tmp_path / "images.txt"
  p.write_text("# header\nimg:1\n\nimg:2  # trailing\nimg:1\n   \n")
  assert load_image_list(str(p)) == ["img:1", "img:2"]
  assert [t.id for t in ImageListSource(str(p)).load()] == ["img:1", "img:2"]
  assert [t.image for t in to_tasks(ImageListSource(str(p)))] == ["img:1", "img:2"]


def test_read_text_gs_prefers_the_storage_client_then_gcloud(monkeypatch):
  calls = []
  class _Blob:
    def download_as_text(self):
      return "img:1\n"
  class _Bucket:
    def blob(self, name):
      calls.append(("blob", name))
      return _Blob()
  class _Client:
    def bucket(self, name):
      calls.append(("bucket", name))
      return _Bucket()
  fake_storage = types.SimpleNamespace(Client=_Client)
  monkeypatch.setitem(__import__("sys").modules, "google", types.SimpleNamespace(cloud=types.SimpleNamespace(storage=fake_storage)))
  monkeypatch.setitem(__import__("sys").modules, "google.cloud", types.SimpleNamespace(storage=fake_storage))
  monkeypatch.setitem(__import__("sys").modules, "google.cloud.storage", fake_storage)
  assert read_text("gs://b/dir/images.txt") == "img:1\n"
  assert calls == [("bucket", "b"), ("blob", "dir/images.txt")]
  # no client library (google.cloud has no storage): the gcloud CLI
  monkeypatch.setitem(__import__("sys").modules, "google.cloud", types.SimpleNamespace())
  monkeypatch.setitem(__import__("sys").modules, "google.cloud.storage", None)
  monkeypatch.setattr(sources.shutil, "which", lambda tool: "/usr/bin/gcloud" if tool == "gcloud" else None)
  ran = []
  monkeypatch.setattr(sources.subprocess, "run",
                      lambda argv, **kw: ran.append(argv) or types.SimpleNamespace(stdout="img:2\n"))
  assert load_image_list("gs://b/x.txt") == ["img:2"]
  assert ran == [["gcloud", "storage", "cat", "gs://b/x.txt"]]
  monkeypatch.setattr(sources.shutil, "which", lambda tool: None)
  with pytest.raises(RuntimeError, match="google-cloud-storage"):
    read_text("gs://b/x.txt")


def test_jsonl_source_streams_local_files_and_keeps_json_line_separators(tmp_path):
  p = tmp_path / "t.jsonl"
  p.write_text('{"image": "img:1", "text": "a\u2028b"}\n{"image": "img:2"}\n', encoding="utf-8")
  tasks = sources.JsonlSource(p).load()                        # PathLike accepted
  assert [t.image for t in tasks] == ["img:1", "img:2"]
  assert tasks[0].metadata["text"] == "a\u2028b"                # U+2028 is not a record end
  assert [t.image for t in sources.JsonlSource(str(p), limit=1).load()] == ["img:1"]


def test_read_text_gs_falls_back_to_the_cli_when_the_client_cannot_authenticate(monkeypatch):
  class _Client:
    def __init__(self):
      raise RuntimeError("no ADC")
  fake_storage = types.SimpleNamespace(Client=_Client)
  monkeypatch.setitem(sys.modules, "google", types.SimpleNamespace(cloud=types.SimpleNamespace(storage=fake_storage)))
  monkeypatch.setitem(sys.modules, "google.cloud", types.SimpleNamespace(storage=fake_storage))
  monkeypatch.setitem(sys.modules, "google.cloud.storage", fake_storage)
  monkeypatch.setattr(sources.shutil, "which", lambda tool: "/usr/bin/gcloud" if tool == "gcloud" else None)
  seen = {}
  def run(argv, **kw):
    seen.update(kw)
    return types.SimpleNamespace(stdout="x\n")
  monkeypatch.setattr(sources.subprocess, "run", run)
  assert sources.read_text("gs://b/f") == "x\n"
  assert seen["check"] is True and seen["encoding"] == "utf-8" and seen["timeout"]

  def boom(argv, **kw):
    raise subprocess.CalledProcessError(1, argv, stderr="AccessDeniedException: 403 forbidden\n")
  monkeypatch.setattr(sources.subprocess, "run", boom)
  with pytest.raises(RuntimeError, match="AccessDeniedException"):
    sources.read_text("gs://b/f")
  monkeypatch.setattr(sources.shutil, "which", lambda tool: None)
  with pytest.raises(RuntimeError, match="no ADC"):             # the client error is named
    sources.read_text("gs://b/f")


def test_extract_image_literal_dotted_keys_multi_element_lists_and_binary():
  assert sources.extract_image({"metadata.docker_image": "img:1"}) == "img:1"   # flattened column
  assert sources.extract_image({"docker_image": ["a:1", "b:2"]}) is None          # not one image
  assert sources.extract_image({"image": b"\x89PNG\r\n", "docker_image": "img:2"}) == "img:2"


def test_row_to_task_strips_the_keys_it_consumed_and_keeps_the_rest():
  t = sources.row_to_task({"instance_id": "i1", "docker_image": "img:1", "repo": "r"}, 0,
                          image_field="docker_image", id_field="instance_id")
  assert (t.id, t.image, t.metadata) == ("i1", "img:1", {"repo": "r"})
  t = sources.row_to_task({"meta": {"docker_image": "img:3"}, "task_id": 7}, 1,
                          image_field="meta.docker_image")
  assert t.id == "7" and t.image == "img:3"
  assert t.metadata == {"meta": {"docker_image": "img:3"}}       # nested keys stay whole


def test_fleet_load_tasks_passes_the_field_options_through(make_cluster):
  from agent_sandbox_rl import ClusterRegistry, FleetConfig, SandboxFleet
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([make_cluster("solo")]))
  tasks = f.load_tasks([{"docker_image": "img:1", "instance_id": "i1"}],
                       image_field="docker_image", id_field="instance_id")
  assert [(t.id, t.image) for t in tasks] == [("i1", "img:1")]
