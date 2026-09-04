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

"""One-shot diagnostic: can this pod publish to the hub, and does it OWN it?

Run this before enabling `--publish-clusterprofile` on a member, and any time
publishing appears to do nothing.

WHY A SEPARATE TOOL
-------------------
Every failure mode in this path is quiet.

* The member wraps publishing in try/except so a broken hub never takes down
  the GCS path -- correct behavior, and it means a misconfiguration shows up
  as *nothing happening*.
* A merge-patch accepts `field_manager` and establishes no ownership at all.
  The data lands. The object looks right. Two managers then silently trample
  each other, and you find out weeks later.
* An absent Workload Identity binding surfaces as a 401 that reads like an
  RBAC problem, and a hub with authorized networks surfaces as a TLS timeout
  that reads like a network blip.

So this asserts each link separately and says which one broke, instead of
leaving you to infer it from a member that is merely quiet.

WHAT IT PROVES, IN ORDER
  1. the token source works        (metadata server reachable, WI bound)
  2. the hub is reachable          (address, CA, authorized networks)
  3. the profile exists            (the operator created it; members cannot)
  4. the apply is accepted         (RBAC: patch on clusterprofiles/<name>/status)
  5. SSA OWNERSHIP was established (managedFields -- the one that matters)

Step 5 is the point. Steps 1-4 can all pass while the write is a merge-patch
that owns nothing, which is exactly the failure this repo has already been
bitten by once.

  fleet-hubcheck --cluster-name cluster-a \
      --hub-kubeconfig /etc/fleet-hub/kubeconfig \
      --hub-token-source gke-metadata

Writes real (dummy-valued) capacity to the live profile, so point it at a
cluster whose profile you are happy to overwrite -- which is any cluster whose
member is about to start publishing anyway. `--dry-run` stops after step 3.
"""

from __future__ import annotations

import argparse
import datetime
import os
import sys

from .inventory import (
    CLUSTERPROFILE_GROUP,
    CLUSTERPROFILE_NAMESPACE,
    CLUSTERPROFILE_PLURAL,
    CLUSTERPROFILE_VERSION,
    PROP_HEARTBEAT,
    PROP_SANDBOX_CAPACITY,
)
from .publisher import DEFAULT_FIELD_MANAGER, ClusterProfilePublisher

OK = "\033[0;32mOK\033[0m"
BAD = "\033[0;31mFAIL\033[0m"


def _step(n: int, what: str) -> None:
    print(f"[{n}/5] {what} ... ", end="", flush=True)


def _ok(detail: str = "") -> None:
    print(f"{OK} {detail}".rstrip())


def _fail(detail: str, hint: str = "") -> None:
    print(f"{BAD} {detail}")
    if hint:
        print(f"      → {hint}", file=sys.stderr)
    sys.exit(1)


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="fleet-hubcheck",
        description="Verify a member can publish to, and own fields on, the hub.")
    p.add_argument("--cluster-name", default=os.environ.get("CLUSTER_NAME"),
                   required=not os.environ.get("CLUSTER_NAME"),
                   help="This cluster's name; must match its ClusterProfile.")
    p.add_argument("--hub-kubeconfig", default=os.environ.get("HUB_KUBECONFIG"))
    p.add_argument("--hub-context", default=os.environ.get("HUB_CONTEXT"))
    p.add_argument("--hub-namespace",
                   default=os.environ.get("HUB_NAMESPACE", CLUSTERPROFILE_NAMESPACE))
    p.add_argument("--hub-api-version",
                   default=os.environ.get("HUB_API_VERSION", CLUSTERPROFILE_VERSION))
    p.add_argument("--hub-token-source", choices=["kubeconfig", "gke-metadata"],
                   default=os.environ.get("HUB_TOKEN_SOURCE", "kubeconfig"))
    p.add_argument("--field-manager", default=DEFAULT_FIELD_MANAGER)
    p.add_argument("--sandbox-capacity", type=int, default=1,
                   help="Value to publish. Deliberately 1 by default so a "
                        "forgotten hubcheck cannot skew placement the way a "
                        "plausible-looking number would.")
    p.add_argument("--dry-run", action="store_true",
                   help="Stop after confirming the profile is readable; write "
                        "nothing.")
    args = p.parse_args(argv)

    pub = ClusterProfilePublisher(
        args.cluster_name,
        namespace=args.hub_namespace,
        kubeconfig=args.hub_kubeconfig,
        context=args.hub_context,
        token_source=args.hub_token_source,
        version=args.hub_api_version,
        field_manager=args.field_manager,
        sandbox_capacity=args.sandbox_capacity,
    )

    # -- 1. credentials ------------------------------------------------------ #
    _step(1, f"acquiring credentials via {args.hub_token_source}")
    try:
        api = pub._apply_api()  # noqa: SLF001 - this tool exists to poke internals
        pub.assert_ssa_configured()
    except Exception as e:
        _fail(str(e),
              "gke-metadata only works inside a GKE pod whose ServiceAccount is "
              "annotated iam.gke.io/gcp-service-account and bound with "
              "roles/iam.workloadIdentityUser.")
    _ok()

    # -- 2 & 3. reachability and the profile --------------------------------- #
    # Folded together: a GET is the cheapest thing that proves both, and
    # separating them would mean two round trips to distinguish errors that the
    # exception text already distinguishes.
    _step(2, f"reading ClusterProfile {args.cluster_name} from the hub")
    try:
        before = api.get_namespaced_custom_object(
            group=CLUSTERPROFILE_GROUP, version=args.hub_api_version,
            namespace=args.hub_namespace, plural=CLUSTERPROFILE_PLURAL,
            name=args.cluster_name)
    except Exception as e:
        status = getattr(e, "status", None)
        hints = {
            401: "the identity did not resolve, so RBAC was never consulted. "
                 "Most likely the GSA lacks container.clusters.get on the hub's "
                 "project — GKE needs that before it will map an IAM identity to "
                 "an RBAC subject, and without it a correct Role and RoleBinding "
                 "still yield 401. Grant roles/container.clusterViewer to the GSA "
                 "(deploy/setup-hub.sh does this; a hub set up before that was "
                 "added will be missing it). Only if that is already granted is "
                 "the Workload Identity binding worth suspecting.",
            403: "authenticated but not authorized. The member Role grants get on "
                 "clusterprofiles/<name>; confirm the GSA email matches the RBAC "
                 "subject exactly.",
            404: f"no ClusterProfile named {args.cluster_name!r} in "
                 f"{args.hub_namespace}. Members are denied `create` on purpose — "
                 f"the operator creates these (deploy/setup-hub.sh step 5).",
        }
        _fail(str(e), hints.get(status,
              "if this hangs rather than erroring, suspect authorized networks on "
              "the hub blocking this cluster's egress."))
    mgrs_before = _managers(before)
    _ok(f"exists; current managers: {', '.join(mgrs_before) or 'none'}")

    _step(3, "checking nothing else owns our properties")
    foreign = [m for m in mgrs_before if m != args.field_manager]
    _ok(f"other managers present: {', '.join(foreign)}" if foreign
        else "we are the only writer")

    if args.dry_run:
        print("\ndry run: stopping before the write.")
        return 0

    # -- 4. the apply -------------------------------------------------------- #
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    _step(4, "applying capacity via Server-Side Apply")
    try:
        pub.publish({
            "cluster": args.cluster_name,
            "updated_at": now,
            "warmpool_depth": 0,
            "warmpool_ready": 0,
            "active_claims": 0,
            "claim_p90_ms": 0.0,
            # Left as None on purpose: publishing 0.0 would read as "idle" and
            # actively attract placement. Absent means unmeasured.
            "node_pressure_score": None,
        })
    except Exception as e:
        status = getattr(e, "status", None)
        hints = {
            403: "the member Role must grant `patch` on clusterprofiles/<name> "
                 "with --subresource=status.",
            409: "another field manager owns a field we tried to set. That is SSA "
                 "working correctly — decide who should own it rather than "
                 "reaching for --force-conflicts.",
            422: "the hub's CRD rejected the body. If the hub runs the real "
                 "upstream CRD, a property name we invented may not pass its "
                 "structural schema.",
        }
        _fail(str(e), hints.get(status, ""))
    _ok()

    # -- 5. ownership -------------------------------------------------------- #
    _step(5, "confirming SSA ownership in managedFields")
    after = api.get_namespaced_custom_object(
        group=CLUSTERPROFILE_GROUP, version=args.hub_api_version,
        namespace=args.hub_namespace, plural=CLUSTERPROFILE_PLURAL,
        name=args.cluster_name)
    mgrs_after = _managers(after)
    if args.field_manager not in mgrs_after:
        _fail(f"{args.field_manager!r} is not in managedFields ({mgrs_after})",
              "The write succeeded but owns nothing, which means it went out as a "
              "merge-patch. assert_ssa_configured() should have caught this — if "
              "it passed and this still fails, the client's content-type "
              "negotiation has changed.")
    _ok(f"owned by {args.field_manager}")

    # Drop entries with no name: a None key reaches k.startswith() below and
    # raises AttributeError, replacing the diagnostic verdict with a traceback.
    props = {q.get("name"): q.get("value")
             for q in (after.get("status") or {}).get("properties") or []
             if q.get("name")}
    print()
    print(f"  {PROP_SANDBOX_CAPACITY} = {props.get(PROP_SANDBOX_CAPACITY)}")
    print(f"  {PROP_HEARTBEAT} = {props.get(PROP_HEARTBEAT)}")
    print(f"  managers = {', '.join(mgrs_after)}")
    surviving = [k for k in props if not k.startswith("agents.x-k8s.io/")]
    if surviving:
        # Proof the list merged per-property instead of being replaced wholesale.
        print(f"  foreign properties preserved = {', '.join(surviving)}")
    print("\nhub publishing verified end to end.")
    return 0


def _managers(obj: dict) -> list[str]:
    seen = []
    for f in (obj.get("metadata") or {}).get("managedFields") or []:
        m = f.get("manager")
        if m and m not in seen:
            seen.append(m)
    return seen


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
