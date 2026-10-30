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

"""Image resolution: docker_image, image map, mirror rewriting, clear errors."""

import json

import pytest

from k8s_agent_sandbox_harbor.images import ImageResolver, ImageUnavailableError, rewrite_registry
from k8s_agent_sandbox_harbor.options import EnvironmentOptions

M = "us-central1-docker.pkg.dev/proj/hub-mirror"


@pytest.mark.parametrize("image,expected", [
    ("python:3.13-slim", f"{M}/library/python:3.13-slim"),
    ("alexgshaw/hello-world:v1", f"{M}/alexgshaw/hello-world:v1"),
    ("docker.io/alexgshaw/hello-world:v1", f"{M}/alexgshaw/hello-world:v1"),
    ("ghcr.io/org/img@sha256:abc", f"{M}/org/img@sha256:abc"),
    ("localhost:5000/img:1", f"{M}/img:1"),
])
def test_rewrite_registry(image, expected):
    assert rewrite_registry(image, M) == expected
    assert rewrite_registry(image, None) == image


def test_resolver_prefers_docker_image_then_map_then_errors(tmp_path):
    env_map = tmp_path / "images.json"
    env_map.write_text(json.dumps({"abc123": "reg/built:abc123", "by-name": "reg/built:name"}))
    r = ImageResolver(EnvironmentOptions(image_map=str(env_map), image_mirror=M))
    assert r.resolve(docker_image="alexgshaw/t:1", environment_id="abc123", environment_name="t",
                     has_dockerfile=True) == f"{M}/alexgshaw/t:1"
    assert r.resolve(docker_image=None, environment_id="abc123", environment_name="t",
                     has_dockerfile=True) == f"{M}/reg/built:abc123"
    assert r.resolve(docker_image=None, environment_id="zzz", environment_name="by-name",
                     has_dockerfile=True) == f"{M}/reg/built:name"
    with pytest.raises(ImageUnavailableError, match="image_map"):
        r.resolve(docker_image=None, environment_id="zzz", environment_name="other",
                  has_dockerfile=True)
    with pytest.raises(ImageUnavailableError, match="docker_image"):
        ImageResolver(EnvironmentOptions()).resolve(docker_image=None, environment_id="z",
                                                    environment_name="o", has_dockerfile=False)
