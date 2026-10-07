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

"""Helpers shared by the provider test modules."""

from __future__ import annotations

import io
import tarfile

from openai_agents_k8s_sandbox import K8sSandboxClientOptions

WARM_POOL = "python-sandbox-pool"
NAMESPACE = "agents"

# The kernel's MAX_ARG_STRLEN: a single execve argument above this fails with E2BIG,
# which is what forces base64 payloads to be chunked.
MAX_ARG_STRLEN = 128 * 1024


def make_options(**overrides: object) -> K8sSandboxClientOptions:
    base: dict[str, object] = {"warm_pool": WARM_POOL, "namespace": NAMESPACE}
    base.update(overrides)
    return K8sSandboxClientOptions(**base)  # type: ignore[arg-type]


def make_tar(members: dict[str, bytes], *, symlinks: dict[str, str] | None = None) -> bytes:
    """Build a tar in memory. Member names are used verbatim, traversal included."""

    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for name, payload in members.items():
            info = tarfile.TarInfo(name=name)
            info.size = len(payload)
            tar.addfile(info, io.BytesIO(payload))
        for name, target in (symlinks or {}).items():
            info = tarfile.TarInfo(name=name)
            info.type = tarfile.SYMTYPE
            info.linkname = target
            tar.addfile(info)
    return buf.getvalue()
