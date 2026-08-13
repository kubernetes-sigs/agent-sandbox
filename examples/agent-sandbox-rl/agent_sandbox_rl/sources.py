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

"""Task sources.

A `Task` is the generic unit the fleet operates on: an ``id`` + a container
``image`` + opaque ``metadata``. Workload-specific loaders (e.g. SWE-bench) live
in ``adapters/`` and just emit `Task`s, keeping the core reusable.
"""

from __future__ import annotations

import json
from typing import Iterable, Protocol, runtime_checkable

from pydantic import BaseModel, Field


class Task(BaseModel):
  """One unit of work.

  Attributes:
    id: Stable, unique task identifier (e.g. a SWE-bench instance id).
    image: Container image the task runs in.
    metadata: Arbitrary extra fields (repo, base_commit, …) — ignored by the
      core, available to callbacks/adapters.
  """

  id: str
  image: str
  metadata: dict = Field(default_factory=dict)


@runtime_checkable
class TaskSource(Protocol):
  """Anything that can produce a list of `Task`s."""

  def load(self) -> list[Task]:
    ...


class ListSource:
  """A `TaskSource` over an in-memory list of `Task`s."""

  def __init__(self, tasks: Iterable[Task]):
    self._tasks = list(tasks)

  def load(self) -> list[Task]:
    return list(self._tasks)


class JsonlSource:
  """A `TaskSource` reading one JSON object per line.

  Args:
    path: Path to a ``.jsonl`` file.
    image_field: Field holding the container image.
    id_field: Field holding the task id (falls back to the row index).
    limit: Cap on the number of tasks. ``None`` (default) = all; ``0`` = none.
  """

  def __init__(self, path: str, *, image_field: str = "image",
               id_field: str = "id", limit: int | None = None):
    self.path = path
    self.image_field = image_field
    self.id_field = id_field
    self.limit = limit

  def load(self) -> list[Task]:
    tasks: list[Task] = []
    if self.limit == 0:                      # explicit "none" (None = all)
      return tasks
    with open(self.path, encoding="utf-8") as fh:
      for i, line in enumerate(fh):
        line = line.strip()
        if not line:
          continue
        row = json.loads(line)
        if self.image_field not in row:
          raise KeyError(
              f"{self.path}:{i + 1}: row is missing image field "
              f"'{self.image_field}'")
        tasks.append(Task(
            id=str(row.get(self.id_field, i)),
            image=row[self.image_field],
            metadata={k: v for k, v in row.items()
                      if k not in (self.image_field, self.id_field)},
        ))
        if self.limit is not None and len(tasks) >= self.limit:
          break
    return tasks


def to_tasks(source) -> list[Task]:
  """Normalize a source into ``list[Task]``.

  Accepts a `TaskSource`, a ``list[Task]``, a ``list[str]`` of images, or a
  ``list[dict]`` (each needing at least an ``image`` key).
  """
  if isinstance(source, TaskSource):
    return source.load()
  if isinstance(source, (list, tuple)):
    out: list[Task] = []
    for i, item in enumerate(source):
      if isinstance(item, Task):
        out.append(item)
      elif isinstance(item, str):
        out.append(Task(id=str(i), image=item))
      elif isinstance(item, dict):
        if "image" not in item:
          raise KeyError(f"task #{i} dict is missing required 'image' key")
        out.append(Task(id=str(item.get("id", i)), image=item["image"],
                        metadata={k: v for k, v in item.items()
                                  if k not in ("id", "image")}))
      else:
        raise TypeError(f"cannot coerce {type(item)!r} to Task")
    return out
  raise TypeError(f"unsupported task source: {type(source)!r}")
