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

"""SWE-bench adapter: dataset -> `Task`s, plus a default probe.

This is the only SWE-bench-specific code in the package; the core stays generic.
`SweBenchSource` needs the ``swebench`` extra (Hugging Face ``datasets``).
"""

from __future__ import annotations

import copy
import logging

from ..sources import Task

logger = logging.getLogger("agent_sandbox_rl.adapters.swebench")

SWEBENCH_DATASET = "R2E-Gym/SWE-Bench-Verified"

# Default per-task probe: prove the task env is live (repo checked out at /testbed).
SWEBENCH_PROBE = [
    "bash", "-lc",
    "echo READY $(hostname); git -C /testbed log -1 --oneline 2>/dev/null"
    " || ls -d /testbed 2>/dev/null || ls /",
]


class SweBenchSource:
  """A `TaskSource` over a SWE-bench-style HF dataset (with a ``docker_image``).

  Args:
    dataset: HF dataset id (default ``R2E-Gym/SWE-Bench-Verified``).
    split: dataset split.
    limit: max tasks. ``None`` (default) = all; ``0`` = none.
    offset: skip the first N rows.
    image_field / id_field: dataset column names.
    keep_row: also store the full dataset row under ``Task.metadata["ds"]``.
      Off by default to keep tasks lean; required by the R2E-Gym adapter
      (`adapters.r2egym`), whose env/reward grading needs the whole row.
  """

  def __init__(self, dataset: str = SWEBENCH_DATASET, split: str = "test",
               limit: int | None = None, offset: int = 0,
               image_field: str = "docker_image", id_field: str = "instance_id",
               keep_row: bool = False):
    self.dataset = dataset
    self.split = split
    self.limit = limit
    self.offset = offset
    self.image_field = image_field
    self.id_field = id_field
    self.keep_row = keep_row

  def load(self) -> list[Task]:
    if self.limit == 0:                 # explicit "none" (None = all)
      return []                         # short-circuit: HF rejects a "[n:n]" slice
    try:
      from datasets import load_dataset
    except ImportError as e:  # pragma: no cover
      raise ImportError(
          "SweBenchSource requires the 'swebench' extra: "
          "pip install 'agent-sandbox-rl[swebench]'") from e
    # Use HF split slicing so a small limit/offset doesn't materialize the whole
    # split. `limit=None` (default) = all; `limit=0` = none (handled above).
    split = self.split
    if self.offset or self.limit is not None:
      end = self.offset + self.limit if self.limit is not None else ""
      split = f"{self.split}[{self.offset}:{end}]"
    logger.info("Loading %s [%s]", self.dataset, split)
    rows = list(load_dataset(self.dataset, split=split))
    tasks = []
    for i, r in enumerate(rows):
      meta = {"repo": r.get("repo", ""), "base_commit": r.get("base_commit", "")}
      if self.keep_row:
        # Deep copy: an owned snapshot so per-task R2E-Gym/reward state never
        # leaks across tasks via shared nested objects in the dataset row.
        meta["ds"] = copy.deepcopy(dict(r))
      tasks.append(Task(id=str(r.get(self.id_field, i)),
                        image=r[self.image_field], metadata=meta))
    logger.info("Loaded %d SWE-bench tasks (%d unique images)",
                len(tasks), len({t.image for t in tasks}))
    return tasks


def swebench_probe(task, handle) -> str:
  """Default ``process_fn``: run the readiness probe inside the sandbox.

  Returns the probe output (``READY <pod>`` + the ``/testbed`` git line). Use as
  ``fleet.run(swebench_probe, ...)``.
  """
  return handle.exec(SWEBENCH_PROBE).strip()
