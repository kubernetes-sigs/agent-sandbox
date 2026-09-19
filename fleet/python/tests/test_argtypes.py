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

"""Interval-validator tests.

Shared by fleetctl and the fleet-member, and the reason it is shared is that
both of them sleep on the value: `Event.wait(interval)` in the member,
`max(0.0, interval - elapsed)` in the CLI. Both return immediately at 0, so a
bad value is not a bad config, it is a hot loop against GCS and an apiserver
that this repo already documents as concurrency-sensitive at density.
"""

from __future__ import annotations

import argparse

import pytest

from agent_sandbox_fleet.argtypes import positive_float


@pytest.mark.parametrize("raw,expected", [
    ("30", 30.0),
    ("0.5", 0.5),
    ("1e3", 1000.0),
])
def test_a_positive_interval_is_accepted(raw, expected):
    assert positive_float(raw) == expected


@pytest.mark.parametrize("raw", ["0", "-1", "-0.001", "0.0"])
def test_a_non_positive_interval_is_rejected(raw):
    with pytest.raises(argparse.ArgumentTypeError, match="positive"):
        positive_float(raw)


def test_nan_is_rejected():
    # The reason the check is `not (val > 0)` and not `val <= 0`: nan fails
    # every comparison, so `val <= 0` is False for nan and would let it through
    # -- and Event.wait(nan) returns immediately.
    with pytest.raises(argparse.ArgumentTypeError):
        positive_float("nan")


def test_inf_is_rejected():
    # inf passes `> 0` happily and produces a loop that runs exactly one pass
    # and then sleeps forever. That looks like a healthy pod.
    with pytest.raises(argparse.ArgumentTypeError):
        positive_float("inf")


def test_a_non_number_names_what_it_got():
    with pytest.raises(argparse.ArgumentTypeError, match="not a number"):
        positive_float("thirty")


def test_the_argparse_error_has_no_chained_cause():
    # B904: argparse prints the exception chain, and the underlying ValueError
    # from float() is noise on a command-line typo.
    try:
        positive_float("thirty")
    except argparse.ArgumentTypeError as e:
        assert e.__cause__ is None
        assert e.__suppress_context__ is True
