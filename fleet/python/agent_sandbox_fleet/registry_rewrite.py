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

"""Registry rewriter — harvested verbatim from
`agent_sandbox_rl/registry_rewrite.py`. Redirect Docker Hub images to an
in-region pull-through cache. Cuts image-pull wall-clock time, the dominant
cost in SWE-bench-style workloads (see the RL PoC's `docs/strategies.md`).
"""

from __future__ import annotations

DOCKER_HOSTS = ("docker.io", "index.docker.io", "registry-1.docker.io")


def _split_host(image: str) -> tuple[str | None, str]:
    head = image.split("/", 1)[0]
    if "/" in image and ("." in head or ":" in head or head == "localhost"):
        return head, image[len(head) + 1:]
    return None, image


def rewrite_image(
    image: str,
    *,
    registry: str,
    project: str = "",
    repo: str = "",
    only_hosts: tuple[str, ...] | None = DOCKER_HOSTS,
) -> str:
    if not registry:
        # An empty registry with empty project/repo makes prefix "" and returns
        # "/library/ubuntu:22.04" -- not a valid reference, and the failure only
        # shows up much later as an ImagePullBackOff on the cluster.
        raise ValueError("registry must be a non-empty host, e.g. 'gcr.io'")
    host, rest = _split_host(image)
    effective = host or "docker.io"
    if only_hosts is not None and effective not in only_hosts:
        return image
    name = rest.split("@", 1)[0].split(":", 1)[0]
    if effective in DOCKER_HOSTS and "/" not in name:
        rest = f"library/{rest}"
    prefix = "/".join(p for p in (registry, project, repo) if p)
    return f"{prefix}/{rest}"


def make_rewriter(
    *,
    registry: str,
    project: str = "",
    repo: str = "",
    only_hosts: tuple[str, ...] | None = DOCKER_HOSTS,
):
    def _rewrite(image: str) -> str:
        return rewrite_image(image, registry=registry, project=project, repo=repo,
                             only_hosts=only_hosts)
    return _rewrite
