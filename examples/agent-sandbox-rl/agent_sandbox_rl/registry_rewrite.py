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

"""Optional image-host rewriting.

Redirect task images at an in-region mirror / pull-through cache (Artifact
Registry, ECR, Harbor, any proxy) without touching the task source — the biggest
lever for cutting `wait_pool_ready` (cross-region pull + Docker Hub rate limits).
Generic: no hard GKE dependency. Wire it via ``fleet.load_tasks(src,
image_rewrite=make_rewriter(...))``.
"""

from __future__ import annotations

# Implicit-or-explicit Docker Hub hosts (the common upstream to redirect).
DOCKER_HOSTS = ("docker.io", "index.docker.io", "registry-1.docker.io")


def _split_host(image: str) -> "tuple[str | None, str]":
  """Split ``image`` into ``(host, remainder)``. The first ``/``-segment is a
  registry host iff it contains a ``.`` or ``:`` or is ``localhost`` (the
  docker reference grammar); otherwise the host is implicit (Docker Hub)."""
  head = image.split("/", 1)[0]
  if "/" in image and ("." in head or ":" in head or head == "localhost"):
    return head, image[len(head) + 1:]
  return None, image


def rewrite_image(image: str, *, registry: str, project: str = "", repo: str = "",
                  only_hosts: "tuple[str, ...] | None" = DOCKER_HOSTS) -> str:
  """Rewrite ``image`` to ``<registry>/<project>/<repo>/<path>:<tag>``.

  Only images whose (implicit or explicit) host is in ``only_hosts`` are
  rewritten; anything already on another registry is returned unchanged (so the
  rewrite is idempotent and safe to apply to mixed task lists). ``only_hosts=None``
  rewrites every image regardless of host. The original repository path and tag
  are preserved under the target (a 1:1 mirror layout, which also matches AR
  remote/pull-through repositories). Docker Hub library shorthand (``ubuntu``) is
  normalized to ``library/ubuntu``."""
  host, rest = _split_host(image)
  effective = host or "docker.io"
  if only_hosts is not None and effective not in only_hosts:
    return image
  # Docker Hub library namespace ("ubuntu" -> "library/ubuntu"), for both the
  # implicit host and an explicit "docker.io/ubuntu" so they mirror to the same
  # path. Strip a tag (":tag") or digest ("@sha256:…") before testing for a "/"
  # so a digest's own colon isn't mistaken for a namespace boundary.
  name = rest.split("@", 1)[0].split(":", 1)[0]
  if effective in DOCKER_HOSTS and "/" not in name:
    rest = f"library/{rest}"
  prefix = "/".join(p for p in (registry, project, repo) if p)
  return f"{prefix}/{rest}"


def make_rewriter(*, registry: str, project: str = "", repo: str = "",
                  only_hosts: "tuple[str, ...] | None" = DOCKER_HOSTS):
  """Return a one-arg ``image -> image`` rewriter bound to a target registry,
  suitable for ``fleet.load_tasks(src, image_rewrite=...)``."""
  def _rewrite(image: str) -> str:
    return rewrite_image(image, registry=registry, project=project, repo=repo,
                         only_hosts=only_hosts)
  return _rewrite
