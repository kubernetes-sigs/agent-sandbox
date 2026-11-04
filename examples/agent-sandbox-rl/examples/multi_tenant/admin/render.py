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

"""Fill the ``${NAME}`` placeholders of a manifest and print it.

A stand-in for ``envsubst`` that needs only python3, and that refuses to leave
a placeholder unfilled: a half-rendered manifest applied to a cluster creates
objects with literal ``${...}`` names. Usage:

  python3 render.py tenant.yaml TENANT=alice QUOTA_PODS=16 ...
"""

from __future__ import annotations

import pathlib
import re
import sys

_PLACEHOLDER = re.compile(r"\$\{([A-Z][A-Z0-9_]*)\}")
_NAME = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


def render(text: str, values: dict[str, str]) -> str:
  """``text`` with every ``${NAME}`` replaced; raises KeyError on a missing name."""
  missing = sorted({m.group(1) for m in _PLACEHOLDER.finditer(text)} - set(values))
  if missing:
    raise KeyError(f"no value for {', '.join(missing)}")
  return _PLACEHOLDER.sub(lambda m: values[m.group(1)], text)


def check_name(value: str, what: str) -> str:
  """A DNS-1123 label (what object names and label values need)."""
  if not _NAME.match(value) or len(value) > 63:
    raise ValueError(f"{what} {value!r} must be a DNS-1123 label (lowercase, digits, '-')")
  return value


def main(argv: list[str]) -> int:
  if len(argv) < 2:
    print(__doc__, file=sys.stderr)
    return 2
  values = dict(a.split("=", 1) for a in argv[2:] if "=" in a)
  if "TENANT" in values:
    check_name(values["TENANT"], "tenant")
  sys.stdout.write(render(pathlib.Path(argv[1]).read_text(), values))
  return 0


if __name__ == "__main__":
  sys.exit(main(sys.argv))
