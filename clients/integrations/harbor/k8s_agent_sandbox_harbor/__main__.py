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

"""Command line: ``python -m k8s_agent_sandbox_harbor cleanup``.

Deletes the claims, pools and templates this package created in a namespace,
in that order, selected by the ``harbor.io/managed-by`` label. A Harbor run
that was killed can leave claims behind; their shutdown time bounds them, this
removes them now.
"""

from __future__ import annotations

import argparse
import asyncio
import sys

from .labels import LABEL_TRIAL, label_value
from .options import EnvironmentOptions
from .templates import CLAIMS, POOLS, TEMPLATES


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m k8s_agent_sandbox_harbor")
    sub = parser.add_subparsers(dest="command", required=True)
    cleanup = sub.add_parser("cleanup", help="delete objects this package created")
    cleanup.add_argument("--namespace", default=None, help="namespace (default: --ek default)")
    cleanup.add_argument("--kube-context", default=None)
    scope = cleanup.add_mutually_exclusive_group(required=True)
    scope.add_argument("--all", action="store_true", help="claims, pools and templates")
    scope.add_argument("--claims", action="store_true", help="claims only, keep the pools warm")
    scope.add_argument("--session", help="the claim of one Harbor session id")
    return parser


async def _cleanup(args: argparse.Namespace) -> int:
    from . import _clients

    values = {}
    if args.namespace:
        values["namespace"] = args.namespace
    if args.kube_context:
        values["kube_context"] = args.kube_context
    options = EnvironmentOptions.from_kwargs(values)
    clients = await _clients.get_clients(options)
    try:
        if args.session:
            counts = await clients.templates.delete_managed(
                plurals=(CLAIMS,), extra_selector=f"{LABEL_TRIAL}={label_value(args.session)}")
        elif args.claims:
            counts = await clients.templates.delete_managed(plurals=(CLAIMS,))
        else:
            counts = await clients.templates.delete_managed(plurals=(CLAIMS, POOLS, TEMPLATES))
    finally:
        await _clients.close_clients()
    for plural, n in counts.items():
        print(f"deleted {n} {plural} in {options.namespace}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "cleanup":
        return asyncio.run(_cleanup(args))
    return 2


if __name__ == "__main__":
    sys.exit(main())
