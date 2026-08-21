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

"""registry_rewrite tests."""

from __future__ import annotations

import pytest

from agent_sandbox_fleet.registry_rewrite import rewrite_image


def test_an_empty_registry_is_rejected_rather_than_silently_wrong():
    # The old behaviour built the prefix by joining registry/project/repo, so an
    # empty registry produced "/library/ubuntu:22.04" -- syntactically not a
    # reference, and the only symptom is an ImagePullBackOff on the cluster
    # minutes later, on every pod of every pool in the plan.
    with pytest.raises(ValueError, match="non-empty host"):
        rewrite_image("ubuntu:22.04", registry="")
