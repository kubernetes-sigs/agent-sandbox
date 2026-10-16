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
import logging
import os
import shutil
import subprocess
from collections.abc import Callable, Sequence
from typing import Iterable, Protocol, runtime_checkable

from pydantic import BaseModel, Field

logger = logging.getLogger("agent_sandbox_rl.sources")

# Where dataset rows keep their container image, in the order tried when a
# source is given no explicit ``image_field``: the SDK's own key, then the
# R2E-Gym / SWE-bench conventions (top level, under ``metadata``, and under the
# environment config a trainer wraps a row in). Dotted paths descend into dicts.
DEFAULT_IMAGE_FIELDS: tuple[str, ...] = (
    "image", "docker_image", "image_name",
    "metadata.image", "metadata.docker_image",
    "metadata.env_config.entry.image", "metadata.env_config.entry.docker_image",
    "env_config.entry.image", "env_config.entry.docker_image",
)
DEFAULT_ID_FIELDS: tuple[str, ...] = ("id", "instance_id", "task_id")

ImageField = "str | Sequence[str] | Callable[[dict], object] | None"


def _dig(row: dict, path: str):
  node = row
  for part in path.split("."):
    if not isinstance(node, dict) or part not in node:
      return None
    node = node[part]
  return node


def _as_image(value) -> str | None:
  """A single image string out of what a dataset hands back: a string, bytes,
  or a one-element list / tuple / array (``tolist``) as columnar datasets do."""
  if value is None:
    return None
  if hasattr(value, "tolist") and not isinstance(value, (str, bytes)):
    value = value.tolist()
  while isinstance(value, (list, tuple)):
    if not value:
      return None
    value = value[0]
  if isinstance(value, bytes):
    value = value.decode("utf-8", errors="replace")
  if isinstance(value, str):
    value = value.strip()
    return value or None
  return None


def extract_image(row: dict, image_field=None) -> str | None:
  """The container image of a dataset ``row``.

  ``image_field`` is one dotted path (``"metadata.docker_image"``), a sequence of
  paths tried in order, a callable ``row -> value``, or ``None`` for
  `DEFAULT_IMAGE_FIELDS`. Returns ``None`` when nothing matches.
  """
  if callable(image_field):
    return _as_image(image_field(row))
  if image_field is None:
    paths: Sequence[str] = DEFAULT_IMAGE_FIELDS
  elif isinstance(image_field, str):
    paths = (image_field,)
  else:
    paths = tuple(image_field)
  for path in paths:
    img = _as_image(_dig(row, path))
    if img:
      return img
  return None


def _row_id(row: dict, id_field, default) -> str:
  if callable(id_field):
    v = id_field(row)
    return str(v) if v is not None else str(default)
  paths = DEFAULT_ID_FIELDS if id_field is None else (
      (id_field,) if isinstance(id_field, str) else tuple(id_field))
  for path in paths:
    v = _dig(row, path)
    if v is not None and not isinstance(v, (dict, list)):
      return str(v)
  return str(default)


def row_to_task(row: dict, index, *, image_field=None, id_field=None) -> Task:
  """`Task` for one dataset row (see `extract_image`); the whole row stays in
  ``metadata`` minus the top-level ``id`` and ``image`` keys. Raises ``KeyError``
  when no image field matches."""
  image = extract_image(row, image_field)
  if not image:
    tried = (image_field if isinstance(image_field, str) else
             "callable" if callable(image_field) else
             ", ".join(image_field or DEFAULT_IMAGE_FIELDS))
    raise KeyError(f"task #{index} row has no container image (tried: {tried})")
  return Task(id=_row_id(row, id_field, index), image=image,
              metadata={k: v for k, v in row.items() if k not in ("id", "image")})


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

  def __init__(self, path: str, *, image_field=None, id_field=None,
               limit: int | None = None):
    self.path = path
    self.image_field = image_field
    self.id_field = id_field
    self.limit = limit

  def load(self) -> list[Task]:
    tasks: list[Task] = []
    if self.limit == 0:                      # explicit "none" (None = all)
      return tasks
    for i, line in enumerate(read_text(self.path).splitlines()):
      line = line.strip()
      if not line:
        continue
      row = json.loads(line)
      try:
        tasks.append(row_to_task(row, i, image_field=self.image_field,
                                 id_field=self.id_field))
      except KeyError as e:
        raise KeyError(f"{self.path}:{i + 1}: {e.args[0]}") from None
      if self.limit is not None and len(tasks) >= self.limit:
        break
    return tasks


def read_text(path: str) -> str:
  """Read a local file or a ``gs://`` object as text.

  ``gs://`` goes through ``google.cloud.storage`` when importable (the ambient
  credentials), else the ``gcloud storage cat`` or ``gsutil cat`` command on
  ``PATH``; a clear error names the options when none is available.
  """
  if path.startswith("gs://"):
    bucket, _, blob = path[len("gs://"):].partition("/")
    try:
      from google.cloud import storage  # type: ignore
    except ImportError:
      storage = None
    if storage is not None:
      return storage.Client().bucket(bucket).blob(blob).download_as_text()
    for tool, argv in (("gcloud", ["gcloud", "storage", "cat", path]),
                       ("gsutil", ["gsutil", "cat", path])):
      if shutil.which(tool):
        return subprocess.run(argv, check=True, capture_output=True,
                              text=True).stdout
    raise RuntimeError(
        f"cannot read {path}: install google-cloud-storage, or put gcloud or "
        "gsutil on PATH")
  with open(os.path.expanduser(path), encoding="utf-8") as fh:
    return fh.read()


def load_image_list(path: str) -> list[str]:
  """Image manifest: one image per line, blank lines and ``#`` comments skipped,
  duplicates dropped (first occurrence wins). Local path or ``gs://``."""
  out: list[str] = []
  seen = set()
  for raw in read_text(path).splitlines():
    line = raw.split("#", 1)[0].strip()
    if line and line not in seen:
      seen.add(line)
      out.append(line)
  return out


class ImageListSource:
  """A `TaskSource` over an image manifest (see `load_image_list`): one task per
  image, ``id`` = the image."""

  def __init__(self, path: str):
    self.path = path

  def load(self) -> list[Task]:
    return [Task(id=img, image=img) for img in load_image_list(self.path)]


def to_tasks(source, *, image_field=None, id_field=None) -> list[Task]:
  """Normalize a source into ``list[Task]``.

  Accepts a `TaskSource`, a ``list[Task]``, a ``list[str]`` of images, or a
  ``list[dict]`` of dataset rows. A row's image is found with `extract_image`:
  ``image_field`` names one dotted path, several tried in order, or a callable;
  by default the SDK's ``image`` key and the R2E-Gym / SWE-bench conventions
  (`DEFAULT_IMAGE_FIELDS`), including a one-element array value. ``id_field``
  likewise (default `DEFAULT_ID_FIELDS`, else the row index).
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
        out.append(row_to_task(item, i, image_field=image_field, id_field=id_field))
      else:
        raise TypeError(f"cannot coerce {type(item)!r} to Task")
    return out
  raise TypeError(f"unsupported task source: {type(source)!r}")
