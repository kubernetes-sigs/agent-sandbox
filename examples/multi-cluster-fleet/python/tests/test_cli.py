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

"""fleetctl argument-handling tests."""

from __future__ import annotations

import argparse

from agent_sandbox_fleet import cli


def _route_args(**overrides):
    base = dict(project=None, location=None, kind=False, all=False,
                bucket="fake-bucket", template=None, json=False)
    base.update(overrides)
    return argparse.Namespace(**base)


def test_route_rejects_project_without_location(capsys):
    # Falling through to ctx_naming=None just omits the ready-to-copy
    # `gcloud container clusters get-credentials` line, which reads as "this
    # fleet has no GKE naming" rather than "you forgot a flag".
    rc = cli.cmd_route(_route_args(project="p"))
    assert rc == 2
    assert "--location" in capsys.readouterr().err


def test_route_rejects_location_without_project(capsys):
    rc = cli.cmd_route(_route_args(location="us-central1"))
    assert rc == 2
    assert "--project" in capsys.readouterr().err
