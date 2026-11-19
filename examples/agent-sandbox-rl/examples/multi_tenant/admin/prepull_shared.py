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

"""Pull the shared image list onto the sandbox nodes once, as the admin.

The pre-pull DaemonSet is a namespaced object, so it runs in a namespace of
the admin's (``rl-shared`` by default). The layers it pulls land in each
node's image cache, which is below the namespace: every tenant's sandboxes on
that node then start from the cache. With ``--keep`` the DaemonSet stays, so a
node the autoscaler adds later pulls the list on arrival.

Each cluster has its own node caches, so ``--contexts a,b`` pre-pulls on every
listed cluster, at the same time.

  python admin/prepull_shared.py --images images.txt [--contexts a,b] [--keep]
"""

from __future__ import annotations

import argparse
import concurrent.futures
import logging
import pathlib
import sys

from agent_sandbox_rl import Cluster, ClusterConfig, prepull
from agent_sandbox_rl.sources import load_image_list


def main(argv=None) -> int:
  p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
  p.add_argument("--images",
                 default=str(pathlib.Path(__file__).resolve().parent.parent / "images.txt"))
  p.add_argument("--contexts", "--context", default=None, dest="contexts",
                 help="comma-separated kube contexts (default: the current context)")
  p.add_argument("--namespace", default="rl-shared",
                 help="where the DaemonSet runs (provision.sh creates it)")
  p.add_argument("--node-selector", default=None, metavar="KEY=VALUE",
                 help="only the nodes sandboxes run on, e.g. cloud.google.com/gke-nodepool=sandbox")
  p.add_argument("--runtime-class", default=None,
                 help="schedule like the sandboxes do (gvisor on GKE Sandbox pools)")
  p.add_argument("--image-pull-secret", default=None)
  p.add_argument("--timeout", type=int, default=1800)
  p.add_argument("--keep", action="store_true",
                 help="leave the DaemonSet so new nodes keep pulling the list")
  p.add_argument("-v", "--verbose", action="store_true")
  args = p.parse_args(argv)
  logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                      format="%(asctime)s %(levelname)s %(name)s %(message)s")

  images = load_image_list(args.images)
  if not images:
    p.error(f"no images in {args.images}")
  node_selector = None
  if args.node_selector:
    k, _, v = args.node_selector.partition("=")
    node_selector = {k: v}
  contexts = [c for c in (args.contexts or "").split(",") if c] or [None]

  def one(ctx):
    # An API error on one cluster must not hide the other clusters' results.
    try:
      cluster = Cluster(ClusterConfig(name=ctx or "default", context=ctx,
                                      namespace=args.namespace))
      return prepull(cluster, images, node_selector=node_selector,
                     runtime_class=args.runtime_class,
                     image_pull_secret=args.image_pull_secret, wait=True,
                     timeout=args.timeout, delete_when_done=not args.keep)
    except Exception as e:  # noqa: BLE001 - reported per cluster below
      logging.getLogger("multi_tenant.prepull").error(
          "pre-pull on %s failed: %s", ctx or "current context", e)
      return False

  with concurrent.futures.ThreadPoolExecutor(max_workers=len(contexts)) as pool:
    results = dict(zip(contexts, pool.map(one, contexts)))
  for ctx, ok in results.items():
    print(f"pre-pull of {len(images)} image(s) in {args.namespace}"
          f"{f' on {ctx}' if ctx else ''}: {'done' if ok else 'INCOMPLETE'}"
          + (" (DaemonSet kept)" if args.keep else ""))
  return 0 if all(results.values()) else 1


if __name__ == "__main__":
  sys.exit(main())
