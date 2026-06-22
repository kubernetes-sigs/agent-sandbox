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

import re
import sys
import types
from unittest.mock import MagicMock

from agent_sandbox_rl import SWEBENCH_PROBE, SweBenchSource, swebench_probe


def _install_fake_datasets(monkeypatch, rows):
  mod = types.ModuleType("datasets")

  def fake_load(*a, **k):
    # Honor HF split slicing ("test[1:2]") the way the real datasets lib does,
    # including its rejection of an empty "[n:n]" slice.
    m = re.search(r"\[(\d*):(\d*)\]$", k.get("split", "") or "")
    if m:
      s = int(m.group(1)) if m.group(1) else None
      e = int(m.group(2)) if m.group(2) else None
      sliced = rows[s:e]
      if not sliced:
        raise ValueError(f"Instruction {k.get('split')!r} corresponds to no data!")
      return sliced
    return rows

  mod.load_dataset = fake_load
  monkeypatch.setitem(sys.modules, "datasets", mod)


ROWS = [
    {"instance_id": "astropy__astropy-12907", "docker_image": "img-a", "repo": "astropy/astropy", "base_commit": "abc"},
    {"instance_id": "django__django-10097", "docker_image": "img-b", "repo": "django/django", "base_commit": "def"},
    {"instance_id": "sympy__sympy-1", "docker_image": "img-c", "repo": "sympy/sympy"},
]


def test_swebench_source_maps_rows(monkeypatch):
  _install_fake_datasets(monkeypatch, ROWS)
  tasks = SweBenchSource().load()          # default limit=None → all
  assert [t.id for t in tasks] == [r["instance_id"] for r in ROWS]
  assert [t.image for t in tasks] == ["img-a", "img-b", "img-c"]
  assert tasks[0].metadata == {"repo": "astropy/astropy", "base_commit": "abc"}


def test_swebench_source_limit_zero_is_empty(monkeypatch):
  _install_fake_datasets(monkeypatch, ROWS)
  assert SweBenchSource(limit=0).load() == []   # 0 = none (None = all)


def test_swebench_source_offset_limit(monkeypatch):
  _install_fake_datasets(monkeypatch, ROWS)
  tasks = SweBenchSource(offset=1, limit=1).load()
  assert [t.id for t in tasks] == ["django__django-10097"]


def test_swebench_source_keep_row(monkeypatch):
  _install_fake_datasets(monkeypatch, ROWS)
  tasks = SweBenchSource(keep_row=True).load()
  assert tasks[0].metadata["ds"] == ROWS[0]            # full row for the r2egym adapter
  assert tasks[0].metadata["repo"] == "astropy/astropy"
  # default stays lean
  assert "ds" not in SweBenchSource().load()[0].metadata


def test_swebench_probe_runs_probe_command():
  handle = MagicMock()
  handle.exec.return_value = "READY pod-xyz\nabc123 commit\n"
  out = swebench_probe(object(), handle)
  assert out == "READY pod-xyz\nabc123 commit"
  handle.exec.assert_called_once_with(SWEBENCH_PROBE)
