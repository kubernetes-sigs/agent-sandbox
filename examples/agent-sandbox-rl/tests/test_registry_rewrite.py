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

from agent_sandbox_rl import Task, make_rewriter, rewrite_image
from agent_sandbox_rl.sources import ListSource

AR = "us-docker.pkg.dev"
PROJ = "my-project"
REPO = "my-mirror"


def _rw(image):
  return rewrite_image(image, registry=AR, project=PROJ, repo=REPO)


def test_rewrites_implicit_docker_hub_userrepo():
  assert _rw("slimshetty/swebench-verified:sweb.eval.x86_64.django__django-1") == (
      "us-docker.pkg.dev/my-project/my-mirror/"
      "slimshetty/swebench-verified:sweb.eval.x86_64.django__django-1")


def test_rewrites_docker_hub_library_shorthand():
  assert _rw("ubuntu:22.04") == (
      "us-docker.pkg.dev/my-project/my-mirror/library/ubuntu:22.04")


def test_rewrites_explicit_docker_io_host():
  assert _rw("docker.io/slimshetty/swebench-verified:t") == (
      "us-docker.pkg.dev/my-project/my-mirror/slimshetty/swebench-verified:t")


def test_explicit_docker_io_official_image_normalized_like_implicit():
  # docker.io/ubuntu and ubuntu must mirror to the SAME path (library/ubuntu),
  # so explicit and implicit official refs don't pull from two locations.
  assert _rw("docker.io/ubuntu:22.04") == _rw("ubuntu:22.04") == (
      "us-docker.pkg.dev/my-project/my-mirror/library/ubuntu:22.04")


def test_non_docker_host_left_untouched():
  gcr = "gcr.io/some/img:v1"
  assert _rw(gcr) == gcr
  # already on the target registry -> unchanged (idempotent)
  already = "us-docker.pkg.dev/p/r/x:y"
  assert _rw(already) == already


def test_idempotent_on_second_pass():
  once = _rw("slimshetty/swebench-verified:t")
  assert _rw(once) == once


def test_only_hosts_none_rewrites_everything():
  out = rewrite_image("gcr.io/some/img:v1", registry=AR, project=PROJ,
                      repo=REPO, only_hosts=None)
  assert out == "us-docker.pkg.dev/my-project/my-mirror/some/img:v1"


def test_make_rewriter_binds_target():
  rw = make_rewriter(registry=AR, project=PROJ, repo=REPO)
  assert rw("redis:7") == (
      "us-docker.pkg.dev/my-project/my-mirror/library/redis:7")


def test_digest_refs_normalize_correctly():
  # implicit-hub single-name + digest -> library/ prepended, digest intact
  out = rewrite_image("ubuntu@sha256:abc", registry=AR, project=PROJ,
                      repo=REPO, only_hosts=None)
  assert out == "us-docker.pkg.dev/my-project/my-mirror/library/ubuntu@sha256:abc"
  # namespaced + digest -> no spurious library/
  out2 = rewrite_image("org/app@sha256:def", registry=AR, project=PROJ,
                       repo=REPO, only_hosts=None)
  assert out2 == "us-docker.pkg.dev/my-project/my-mirror/org/app@sha256:def"


def test_load_tasks_applies_rewrite_and_preserves_original():
  from agent_sandbox_rl import FleetConfig, SandboxFleet
  from agent_sandbox_rl.cluster import ClusterRegistry
  # no cluster I/O needed for load_tasks
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([]))
  src = ListSource([Task(id="a", image="slimshetty/swebench-verified:t")])
  tasks = f.load_tasks(src, image_rewrite=make_rewriter(
      registry=AR, project=PROJ, repo=REPO))
  assert tasks[0].image.startswith("us-docker.pkg.dev/my-project/my-mirror/")
  assert tasks[0].metadata["original_image"] == "slimshetty/swebench-verified:t"


def test_load_tasks_rewrite_does_not_mutate_caller_tasks():
  # rewriting must copy, not alias/mutate the caller's Task objects
  from agent_sandbox_rl import FleetConfig, SandboxFleet
  from agent_sandbox_rl.cluster import ClusterRegistry
  orig = Task(id="a", image="slimshetty/swebench-verified:t")
  f = SandboxFleet(FleetConfig(), registry=ClusterRegistry([]))
  f.load_tasks(ListSource([orig]), image_rewrite=make_rewriter(
      registry=AR, project=PROJ, repo=REPO))
  assert orig.image == "slimshetty/swebench-verified:t"   # original untouched
  assert "original_image" not in orig.metadata
