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

import json

import pytest

from agent_sandbox_rl import JsonlSource, ListSource, Task, to_tasks


def test_to_tasks_from_images():
  tasks = to_tasks(["img-a", "img-b"])
  assert [t.image for t in tasks] == ["img-a", "img-b"]
  assert [t.id for t in tasks] == ["0", "1"]


def test_to_tasks_from_dicts():
  tasks = to_tasks([{"id": "x", "image": "img", "repo": "r/r"}])
  assert tasks[0].id == "x"
  assert tasks[0].metadata == {"repo": "r/r"}


def test_to_tasks_passthrough_tasks():
  t = Task(id="a", image="i")
  assert to_tasks([t])[0] is t


def test_to_tasks_listsource():
  src = ListSource([Task(id="a", image="i")])
  assert to_tasks(src)[0].id == "a"


def test_to_tasks_rejects_bad():
  with pytest.raises(TypeError):
    to_tasks(123)
  with pytest.raises(TypeError):
    to_tasks([3.5])


def test_jsonl_source(tmp_path):
  p = tmp_path / "tasks.jsonl"
  p.write_text("\n".join(json.dumps(r) for r in [
      {"instance_id": "i1", "docker_image": "img1", "repo": "a"},
      {"instance_id": "i2", "docker_image": "img2", "repo": "b"},
  ]))
  tasks = JsonlSource(str(p), image_field="docker_image",
                      id_field="instance_id").load()
  assert [t.id for t in tasks] == ["i1", "i2"]
  assert tasks[0].image == "img1"
  assert tasks[0].metadata["repo"] == "a"
  # limit semantics: None (default) = all, 0 = none, N = first N
  kw = dict(image_field="docker_image", id_field="instance_id")
  assert JsonlSource(str(p), limit=0, **kw).load() == []
  assert len(JsonlSource(str(p), limit=1, **kw).load()) == 1
  assert len(JsonlSource(str(p), **kw).load()) == 2


def test_to_tasks_dict_missing_image_raises():
  import pytest
  from agent_sandbox_rl import to_tasks
  with pytest.raises(KeyError, match="image"):
    to_tasks([{"id": "x"}])
