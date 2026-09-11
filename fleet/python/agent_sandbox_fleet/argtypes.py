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

"""Shared argparse ``type=`` converters.

Its own module, with no package-local imports, because both entrypoints need
these: ``cli`` (fleetctl) pulls in planner and inventory, while
``fleet_member`` deliberately imports nothing but ``objectstore`` so the member
pod stays light. Neither can import the other.
"""

from __future__ import annotations

import argparse


def positive_float(raw: str) -> float:
    """argparse type for poll/loop intervals. Rejects <= 0, nan and inf.

    A non-positive interval turns every one of our loops into a spin: the waits
    are ``Event.wait(interval)`` or ``max(0.0, interval - elapsed)``, both of
    which return immediately at 0, so the loop hammers GCS and the apiserver
    with no delay between passes.

    The comparison is ``not (val > 0)`` rather than ``val <= 0`` on purpose:
    nan fails every comparison, so ``val <= 0`` is False for nan and would let
    it through. inf is rejected separately -- it passes ``> 0`` happily and
    produces a loop that runs one pass and then sleeps forever.
    """
    try:
        val = float(raw)
    except (ValueError, OverflowError):
        # from None: the underlying ValueError is noise on a CLI arg error, and
        # argparse prints the chain.
        raise argparse.ArgumentTypeError(f"{raw!r} is not a number") from None
    if not (val > 0) or val == float("inf"):
        raise argparse.ArgumentTypeError(
            f"must be a positive number of seconds, got {raw!r}"
        )
    return val
