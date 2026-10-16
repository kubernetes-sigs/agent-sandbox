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


def test_to_tasks_rows_use_conventions_and_keep_the_row_as_metadata():
  rows = [{"instance_id": "django-1", "docker_image": "img:1", "repo": "django"},
          {"id": "x", "image": "img:2"},
          {"metadata": {"docker_image": "img:3"}, "task_id": 7}]
  tasks = to_tasks(rows)
  assert [(t.id, t.image) for t in tasks] == [("django-1", "img:1"), ("x", "img:2"), ("7", "img:3")]
  assert tasks[0].metadata == {"instance_id": "django-1", "docker_image": "img:1", "repo": "django"}
  assert tasks[1].metadata == {}
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
