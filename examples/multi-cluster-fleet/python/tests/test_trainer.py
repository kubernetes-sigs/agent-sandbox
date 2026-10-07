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

"""Tests for agent_sandbox_fleet.trainer."""

from __future__ import annotations

import pytest

from agent_sandbox_fleet.trainer import parse_size

KIB = 1024
MIB = 1024 * KIB
GIB = 1024 * MIB


# --------------------------------------------------------------------------- #
# Suffix matching used to iterate a dict, so "1MiB" matched the "B" entry
# first and float("1Mi") raised. --delta-size defaults to "1MiB", so the
# trainer crashed on its own default flags. Longest suffix must win.
# --------------------------------------------------------------------------- #

@pytest.mark.parametrize("text,expected", [
    ("1B", 1),
    ("100B", 100),
    ("1KiB", KIB),
    ("512KiB", 512 * KIB),
    ("1MiB", MIB),
    ("1GiB", GIB),
    ("2GiB", 2 * GIB),
    ("1.5MiB", int(1.5 * MIB)),
    ("1024", 1024),  # bare integer, no suffix
    ("  1MiB  ", MIB),  # surrounding whitespace
])
def test_parse_size(text, expected):
    assert parse_size(text) == expected


def test_parse_size_matches_the_documented_default():
    # trainer.main()'s --delta-size default. If this raises, the trainer is
    # unrunnable without flags.
    assert parse_size("1MiB") == MIB
