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

"""`fleetctl` — the operator CLI.

Verbs:
  fleetctl apply -f fleet-spec.yaml    Push a spec to GCS and produce assignments
  fleetctl apply -f ... --loop         Same, but re-plan continuously every N seconds
  fleetctl status                      Print per-cluster capacity + assignment summary
  fleetctl show-assignments            Dump the current assignments.json
  fleetctl show-registry               Dump the live registry that the planner sees
  fleetctl route --template=X          Print which cluster to hit for a given template
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import sys
import threading
import time

import yaml

from . import inventory, planner
from .argtypes import positive_float as _positive_float
from .objectstore import GCS, Paths
from .resolver import (
    ClusterResolver,
    ResolverError,
    gke_context_naming,
    kind_context_naming,
)

log = logging.getLogger("agent_sandbox_fleet.cli")


def _bucket_from(args) -> str:
    # args.bucket may be absent when SUPPRESS is used on the subparser default
    # (see main()). getattr fallback keeps this safe.
    b = getattr(args, "bucket", None) or os.environ.get("FLEET_BUCKET")
    if not b:
        sys.exit("--bucket or $FLEET_BUCKET is required")
    return b



def _optional_bucket(args) -> str | None:
    """Bucket if one is configured, else None. Does not exit."""
    return getattr(args, "bucket", None) or os.environ.get("FLEET_BUCKET") or None


def _inventory_kind(args) -> str:
    return getattr(args, "inventory", None) or os.environ.get("FLEET_INVENTORY", "gcs")


def _provider_from(args, gcs: GCS | None, paths: Paths | None = None):
    """Build the inventory provider selected by --inventory.

    GCS stays the default and the fallback. `clusterprofile` reads
    SIG-Multicluster ClusterProfile CRs from a hub instead — the spec and the
    assignments still travel through GCS either way.
    """
    kind = _inventory_kind(args)
    try:
        return inventory.get_inventory(
            kind,
            gcs=gcs,
            paths=paths,
            hub_kubeconfig=getattr(args, "hub_kubeconfig", None),
            hub_context=getattr(args, "hub_context", None),
            hub_namespace=getattr(args, "hub_namespace", None)
            or inventory.CLUSTERPROFILE_NAMESPACE,
            hub_token_source=getattr(args, "hub_token_source", None)
            or os.environ.get("HUB_TOKEN_SOURCE")
            or "kubeconfig",
            cluster_manager=getattr(args, "cluster_manager", None),
            require_heartbeat=getattr(args, "require_heartbeat", False),
            version=getattr(args, "hub_api_version", None)
            or os.environ.get("HUB_API_VERSION")
            or inventory.CLUSTERPROFILE_VERSION,
        )
    except ValueError as e:
        sys.exit(str(e))


# --------------------------------------------------------------------------- #
# apply — one-shot or continuous loop
# --------------------------------------------------------------------------- #

def cmd_apply(args) -> int:
    """Apply a FleetSpec. With --loop, re-plan every --loop-interval seconds
    so cluster failures shift assignments without operator intervention.
    """
    gcs = GCS(_bucket_from(args))
    provider = _provider_from(args, gcs)

    if not args.loop:
        return _apply_once(gcs, args.file, args.quiet, provider,
                           generation=getattr(args, "generation", None))

    # --generation is a one-shot escape hatch. Honouring it in --loop mode would
    # make cycle 2 fail its own monotonicity check against the generation cycle
    # 1 just published, so every cycle after the first would error out and the
    # loop would stop re-planning while still looking alive.
    if getattr(args, "generation", None) is not None:
        log.warning("--generation is ignored in --loop mode; generations are "
                    "derived from the published assignments each cycle")

    stop = threading.Event()

    def _shutdown(signum, _frame):
        log.info("received signal %d — will exit after current cycle", signum)
        stop.set()

    signal.signal(signal.SIGINT, _shutdown)
    signal.signal(signal.SIGTERM, _shutdown)

    log.info(
        "starting continuous planner loop: interval=%.1fs spec=%s bucket=%s",
        args.loop_interval, args.file, gcs.bucket_name,
    )
    cycle = 0
    while not stop.is_set():
        cycle += 1
        started = time.time()
        try:
            rc = _apply_once(gcs, args.file, quiet=True, provider=provider)
            elapsed = time.time() - started
            if rc == 0:
                log.info("cycle %d ok in %.2fs", cycle, elapsed)
            else:
                log.error("cycle %d returned rc=%d in %.2fs", cycle, rc, elapsed)
        except Exception:  # noqa: BLE001 — never let one bad cycle kill the loop
            log.exception("cycle %d failed; will retry next interval", cycle)
        # Sleep the remainder of the interval. wait() returns True if stopped.
        remaining = max(0.0, args.loop_interval - (time.time() - started))
        if stop.wait(remaining):
            break
    log.info("planner loop exited after %d cycles", cycle)
    return 0


def _apply_once(gcs: GCS, spec_path: str, quiet: bool, provider=None,
                generation: int | None = None) -> int:
    with open(spec_path) as f:
        body = yaml.safe_load(f)
    spec = planner.FleetSpec.model_validate(body)
    assn = planner.apply(gcs, spec, provider=provider, generation=generation)
    if not quiet:
        print(json.dumps(assn.model_dump(), indent=2))
    return 0


# --------------------------------------------------------------------------- #
# status / show-assignments / show-registry
# --------------------------------------------------------------------------- #

def cmd_status(args) -> int:
    gcs = GCS(_bucket_from(args))
    paths = Paths()

    spec_raw = gcs.get_json(paths.spec)
    assn_raw = gcs.get_json(paths.assignments)
    if spec_raw is None:
        print("no fleet spec published yet (fleetctl apply -f ...)")
        return 1
    spec = planner.FleetSpec.model_validate(spec_raw)

    reg = _provider_from(args, gcs, paths).load(spec.cluster_weights)

    fmt = "{:<14} {:<6} {:<8} {:<8} {:<8} {:<10} {:<8}"
    print(fmt.format("cluster", "age_s", "wp_depth", "wp_ready", "claims",
                     "p90_ms", "pools"))
    print(fmt.format("-" * 14, "-" * 6, "-" * 8, "-" * 8, "-" * 8, "-" * 10, "-" * 8))
    assn = planner.Assignments.model_validate(assn_raw) if assn_raw else None
    for name in sorted(reg.clusters):
        c = reg.clusters[name]
        n_pools = len(assn.clusters.get(name).pools) if assn and name in assn.clusters else 0
        age = int(c.report_age_s) if c.report_age_s < 1e8 else "STALE"
        # "-" not "0": the member reports None when it could not measure claims
        # (light mode, or the list failed), and 0 would read as a healthy idle.
        claims = "-" if c.active_claims is None else str(c.active_claims)
        print(fmt.format(name, str(age), c.warmpool_depth, c.warmpool_ready,
                         claims, f"{c.claim_p90_ms:.1f}", n_pools))

    if assn is not None:
        print(f"\nassignments generation: {assn.generation}  updated_at: {assn.updated_at}")
    wm = gcs.get_json(paths.weight_manifest)
    if wm:
        print(f"weight stream '{wm.get('weight_stream')}' current_version: {wm.get('current_version')}")
    return 0


def cmd_show_assignments(args) -> int:
    gcs = GCS(_bucket_from(args))
    data = gcs.get_json(Paths().assignments)
    if data is None:
        print("no assignments.json in bucket")
        return 1
    print(json.dumps(data, indent=2))
    return 0


def cmd_show_registry(args) -> int:
    # With ClusterProfile inventory this command can run against the hub alone.
    # A bucket is only needed to pick up operator weight OVERRIDES from the
    # published spec; without one, weights derive from each cluster's own
    # sandbox-capacity property, which is the point of consuming ClusterProfiles.
    kind = _inventory_kind(args)
    bucket = _optional_bucket(args)
    if bucket is None and kind == "gcs":
        sys.exit("--bucket or $FLEET_BUCKET is required for --inventory=gcs")

    gcs = GCS(bucket) if bucket else None
    weights: dict[str, float] = {}
    origin = "derived from each cluster's agents.x-k8s.io/sandbox-capacity"
    if gcs is not None:
        spec_raw = gcs.get_json(Paths().spec)
        weights = (spec_raw or {}).get("cluster_weights", {})
        if weights:
            origin = "cluster_weights from the published spec"
    print(f"# weights: {origin}", file=sys.stderr)

    reg = _provider_from(args, gcs).load(weights)
    out = {name: {
        "weight": c.weight,
        "report_age_s": round(c.report_age_s, 1),
        "warmpool_depth": c.warmpool_depth,
        "warmpool_ready": c.warmpool_ready,
        "active_claims": c.active_claims,
        "claim_p90_ms": c.claim_p90_ms,
        "node_pressure_score": c.node_pressure_score,
        "fresh": c.report_age_s <= reg.max_report_age_s,
    } for name, c in reg.clusters.items()}
    print(json.dumps(out, indent=2))
    return 0


# --------------------------------------------------------------------------- #
# route — the onboarding-ergonomics command
# --------------------------------------------------------------------------- #

def cmd_route(args) -> int:
    """Print which cluster hosts the given template. Solves the "new engineer
    joins and wants to know which cluster to hit" onboarding question.
    """
    # Choose a context-naming function so we can print a ready-to-copy
    # `gcloud get-credentials` command. If --project + --location are set,
    # use GKE naming; otherwise fall back to kind naming.
    #
    # A half-specified pair is an error, not a fallback: dropping silently to
    # ctx_naming=None just omits the `gcloud container clusters get-credentials`
    # line from the output, which reads as "this fleet has no GKE naming" rather
    # than "you forgot a flag".
    if bool(args.project) != bool(args.location):
        missing = "location" if args.project else "project"
        print(f"--project and --location must be given together (missing "
              f"--{missing})", file=sys.stderr)
        return 2

    bucket = _bucket_from(args)
    ctx_naming = None
    if args.project and args.location:
        ctx_naming = gke_context_naming(args.project, args.location)
    elif args.kind:
        ctx_naming = kind_context_naming()

    resolver = ClusterResolver(bucket, context_naming=ctx_naming)

    # --all: print every template + its cluster(s). Useful for a "phone book"
    # dump when the engineer doesn't know the template name yet.
    if args.all:
        templates = resolver.all_templates()
        if not templates:
            print("no templates in current assignment; run `fleetctl apply` first",
                  file=sys.stderr)
            return 1
        out = {}
        for t in templates:
            matches = resolver.list_matches(t)
            out[t] = [
                {
                    "cluster": m.cluster, "warmpool": m.warmpool,
                    "replicas": m.replicas, "context": m.context_name,
                }
                for m in matches
            ]
        print(json.dumps(out, indent=2))
        return 0

    if not args.template:
        print("either --template=NAME or --all is required", file=sys.stderr)
        return 2

    try:
        resolved = resolver.resolve(args.template, strategy=args.strategy)
    except ResolverError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    if args.format == "json":
        payload = {
            "cluster": resolved.cluster,
            "template": resolved.template,
            "warmpool": resolved.warmpool,
            "replicas": resolved.replicas,
            "image": resolved.image,
            "generation": resolved.generation,
            "context_name": resolved.context_name,
        }
        if args.project and args.location:
            payload["gcloud_get_credentials"] = resolved.get_credentials_cmd(
                project=args.project, location=args.location,
            )
        print(json.dumps(payload, indent=2))
        return 0

    # text format — human-friendly
    print(f"template : {resolved.template}")
    print(f"cluster  : {resolved.cluster}")
    print(f"warmpool : {resolved.warmpool}")
    print(f"replicas : {resolved.replicas}")
    if resolved.image:
        print(f"image    : {resolved.image}")
    if resolved.context_name:
        print(f"context  : {resolved.context_name}")
    if args.project and args.location:
        print()
        print("# Run this to make the context available locally:")
        print(f"  {resolved.get_credentials_cmd(project=args.project, location=args.location)}")
    return 0


# --------------------------------------------------------------------------- #
# entrypoint
# --------------------------------------------------------------------------- #

def build_parser() -> argparse.ArgumentParser:
    """Construct the full `fleetctl` parser.

    Split out from `main` so tests can exercise flag wiring without running a
    command or touching GCS.
    """
    p = argparse.ArgumentParser(prog="fleetctl", description="Multi-cluster sandbox fleet operator CLI.")
    # Top-level --bucket for the classic `fleetctl --bucket=X subcommand ...` order.
    p.add_argument("--bucket", help="GCS bucket (default: $FLEET_BUCKET)")
    sub = p.add_subparsers(dest="cmd", required=True)

    # Shared subparser parent so `fleetctl subcommand --bucket=X ...` ALSO works.
    # Argparse doesn't inherit top-level flags into subparsers by default, so we
    # attach the same flag via a common parent. Default=SUPPRESS matters: without
    # it, the subparser overwrites args.bucket with None when the flag is absent
    # from the subcommand, wiping out any value set at the top level.
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument(
        "--bucket", help="GCS bucket (default: $FLEET_BUCKET)",
        default=argparse.SUPPRESS,
    )
    # Inventory source. GCS capacity reports are the default and need no hub;
    # `clusterprofile` reads SIG-Multicluster ClusterProfile CRs instead. The
    # spec and assignments still travel through GCS either way.
    common.add_argument(
        "--inventory", choices=["gcs", "clusterprofile"],
        default=argparse.SUPPRESS,
        help="Where cluster inventory comes from (default: gcs, or "
             "$FLEET_INVENTORY)",
    )
    common.add_argument(
        "--hub-kubeconfig", default=argparse.SUPPRESS,
        help="Kubeconfig for the hub holding ClusterProfile CRs "
             "(--inventory=clusterprofile)",
    )
    common.add_argument(
        "--hub-context", default=argparse.SUPPRESS,
        help="Kubeconfig context for the hub (--inventory=clusterprofile)",
    )
    common.add_argument(
        "--hub-namespace", default=argparse.SUPPRESS,
        help=f"Namespace holding ClusterProfile CRs "
             f"(default: {inventory.CLUSTERPROFILE_NAMESPACE})",
    )
    common.add_argument(
        "--hub-token-source", default=argparse.SUPPRESS,
        choices=["kubeconfig", "gke-metadata"],
        help="How to authenticate to the hub. 'gke-metadata' authenticates as "
             "the pod's Workload Identity service account and ignores any "
             "credentials in --hub-kubeconfig, which lets that kubeconfig be a "
             "credential-free ConfigMap. Env: HUB_TOKEN_SOURCE",
    )
    common.add_argument(
        "--hub-api-version", default=argparse.SUPPRESS,
        metavar="VERSION",
        help=f"Served ClusterProfile API version (default: "
             f"{inventory.CLUSTERPROFILE_VERSION}; upstream also serves "
             f"v1alpha2 and deprecates v1alpha1). Env: HUB_API_VERSION",
    )
    common.add_argument(
        "--cluster-manager", default=argparse.SUPPRESS,
        help="Only read ClusterProfiles labelled with this cluster manager "
             f"({inventory.CLUSTER_MANAGER_LABEL})",
    )
    common.add_argument(
        "--require-heartbeat", action="store_true", default=argparse.SUPPRESS,
        help=f"Treat a ClusterProfile with no {inventory.PROP_HEARTBEAT} "
             "property as stale. Without it, a silently dead cluster whose "
             "ControlPlaneHealthy condition never flipped still looks fresh.",
    )

    # apply
    a = sub.add_parser("apply", parents=[common],
                       help="Apply a FleetSpec (writes spec + assignments to GCS)")
    a.add_argument("-f", "--file", required=True, help="Path to fleet-spec.yaml")
    a.add_argument(
        "--loop", action="store_true",
        help="Run continuously — re-plan every --loop-interval seconds. Fixes "
             "the 'cluster dies but no re-apply' reactivity gap.",
    )
    a.add_argument(
        "--loop-interval", type=_positive_float, default=60.0,
        help="Seconds between re-plans in --loop mode (default 60)",
    )
    a.add_argument(
        "--quiet", action="store_true",
        help="Don't print the assignments JSON after apply (implied by --loop)",
    )
    a.add_argument(
        "--generation", type=int, default=None,
        help="Force a specific assignment generation (replay / disaster "
             "recovery). Normally derived by incrementing the published one. "
             "Must be greater than what is in the bucket, or members ignore it. "
             "Ignored in --loop mode, which would otherwise republish the same "
             "generation every cycle.",
    )
    a.set_defaults(func=cmd_apply)

    # status
    s = sub.add_parser("status", parents=[common], help="Print per-cluster status table")
    s.set_defaults(func=cmd_status)

    # show-assignments
    sa = sub.add_parser("show-assignments", parents=[common],
                        help="Dump the current assignments.json")
    sa.set_defaults(func=cmd_show_assignments)

    # show-registry
    sr = sub.add_parser("show-registry", parents=[common],
                        help="Dump the live PlannerRegistry")
    sr.set_defaults(func=cmd_show_registry)

    # route
    r = sub.add_parser(
        "route", parents=[common],
        help="Print which cluster hosts a template + a ready-to-run gcloud "
             "get-credentials command. Meant for onboarding a new engineer.",
    )
    r.add_argument(
        "--template", help="Template name to look up (omit and pass --all for a phone-book dump)",
    )
    r.add_argument(
        "--all", action="store_true",
        help="Dump every template in the current assignment + its cluster(s)",
    )
    r.add_argument(
        "--strategy", default="round-robin",
        choices=["first", "round-robin", "hash"],
        help="How to pick among clusters when a template lives on more than one "
             "(default: round-robin)",
    )
    r.add_argument(
        "--format", default="text", choices=["text", "json"],
        help="Output format (default: text)",
    )
    r.add_argument(
        "--project",
        help="GCP project (for building a `gcloud get-credentials` command)",
    )
    r.add_argument(
        "--location",
        help="GCP zone (Standard) or region (Autopilot). Required alongside --project.",
    )
    r.add_argument(
        "--kind", action="store_true",
        help="Use kind context naming (kind-<cluster>) instead of GKE naming",
    )
    r.set_defaults(func=cmd_route)

    return p


def main() -> int:
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(name)s %(levelname)s %(message)s",
        stream=sys.stderr,
    )
    args = build_parser().parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
