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

"""Cluster resolver + multi-cluster SandboxClient facade.

This is the seam between the caller and the fleet: consumers should
call ``client.create_sandbox(template="foo")`` and have the fleet layer
resolve which cluster to hit, load the right kube-context, and dispatch
under the hood — without writing per-cluster glue.

Two public surfaces:

1. :func:`resolve_cluster` — pure lookup. Reads ``fleet/assignments.json``
   from the fleet bucket and returns which cluster hosts the given
   template. Use this if you're driving your own SandboxClient(s).

2. :class:`FleetSandboxClient` — thin wrapper over
   ``k8s_agent_sandbox.SandboxClient`` that resolves + caches per-cluster
   clients lazily. Same interface shape as ``SandboxClient`` for the two
   methods a fleet consumer typically uses (``create_sandbox``,
   ``delete_sandbox``).

Design notes:

* Assignments are cached with an on-demand staleness check
  (``refresh_interval_s``, default 60s). No background threads.
* Per-cluster SandboxClient construction is serialised on a lock because
  ``kubernetes.config.load_kube_config`` mutates global state.
* Round-robin is the default resolution strategy so templates that live on
  multiple clusters (spread-first, min_clusters) get load-balanced.
  ``strategy="first"`` gives deterministic (sorted-by-name) placement for
  reproducible tests. ``strategy="hash"`` gives deterministic
  per-template pinning (matches image-affinity behavior on the client
  side).
* GCS reads retry with exponential backoff on transient errors so a
  bucket blip doesn't blow up a claim path.
"""

from __future__ import annotations

import hashlib
import itertools
import logging
import threading
import time
from dataclasses import dataclass
from typing import Any, Callable, Iterable, Optional

from .objectstore import GCS, Paths

log = logging.getLogger("agent_sandbox_fleet.resolver")

# ----------------------------------------------------------------------------
# Exceptions
# ----------------------------------------------------------------------------

class ResolverError(RuntimeError):
    """Base class for resolver errors."""


class TemplateNotAssignedError(ResolverError):
    """Raised when no cluster in the current assignment hosts the template.

    Typical causes:
    - Operator hasn't run ``fleetctl apply`` yet.
    - The template name is wrong (typo, or a name that isn't in the spec).
    - The spec was applied but the referenced SandboxTemplate CR is missing
      on every cluster, so no fleet-member could create a warmpool for it.
    """


class ClusterUnavailableError(ResolverError):
    """Raised when we resolved to a cluster but cannot reach it.

    Usually because the caller hasn't run
    ``gcloud container clusters get-credentials`` for that cluster, so the
    expected context isn't in ``~/.kube/config``.
    """


class AssignmentsMissingError(ResolverError):
    """Raised when ``fleet/assignments.json`` doesn't exist in the bucket."""


# ----------------------------------------------------------------------------
# Data types
# ----------------------------------------------------------------------------

@dataclass(frozen=True)
class ResolvedCluster:
    """Result of resolving a template to a cluster.

    ``context_name`` is the caller-supplied kubeconfig context to use when
    constructing a per-cluster client. On GKE this defaults to
    ``gke_<project>_<location>_<cluster>``; kind uses ``kind-<cluster>``.
    ``None`` means the caller must supply their own via
    ``context_naming=`` on the resolver.
    """
    cluster: str
    template: str
    warmpool: str
    replicas: int
    image: Optional[str]
    generation: int
    context_name: Optional[str]

    def get_credentials_cmd(
        self, project: str | None = None, location: str | None = None,
    ) -> str:
        """Return the ``gcloud container clusters get-credentials`` command a
        human would run to make :attr:`context_name` available locally. Purely
        informational — the resolver doesn't shell out to gcloud."""
        parts = [
            "gcloud", "container", "clusters", "get-credentials", self.cluster,
        ]
        if location:
            parts.extend(["--zone", location] if _looks_zonal(location)
                         else ["--region", location])
        if project:
            parts.extend(["--project", project])
        return " ".join(parts)


def _looks_zonal(loc: str) -> bool:
    """Very cheap zone-vs-region check. Zones look like ``us-central1-a``;
    regions look like ``us-central1``. Trailing single-letter suffix wins."""
    return len(loc) >= 2 and loc[-2] == "-" and loc[-1].isalpha()


# ----------------------------------------------------------------------------
# Resolver
# ----------------------------------------------------------------------------

# Type alias for the caller-supplied "given a cluster name, tell me the local
# kubeconfig context that reaches it" function. Defaults per platform below.
ContextNaming = Callable[[str], str]


def gke_context_naming(project: str, location: str) -> ContextNaming:
    """Standard GKE context naming: ``gke_<project>_<location>_<cluster>``.

    Matches what ``gcloud container clusters get-credentials`` writes.
    ``location`` can be a zone (Standard) or region (Autopilot); the string
    is copied verbatim so pass whatever gcloud used at get-credentials time.
    """
    def _naming(cluster: str) -> str:
        return f"gke_{project}_{location}_{cluster}"
    return _naming


def kind_context_naming() -> ContextNaming:
    """kind context naming: ``kind-<cluster>``. Used by the local dev demo."""
    def _naming(cluster: str) -> str:
        return f"kind-{cluster}"
    return _naming


class ClusterResolver:
    """Reads ``fleet/assignments.json`` from GCS and answers "which cluster
    hosts template X?".

    Not thread-hostile — internal cache is guarded by a lock and refresh is
    idempotent under contention.
    """

    def __init__(
        self,
        bucket: str,
        *,
        paths: Paths | None = None,
        refresh_interval_s: float = 60.0,
        context_naming: ContextNaming | None = None,
        gcs: GCS | None = None,
        retries: int = 3,
        retry_base_delay_s: float = 0.5,
    ):
        self._bucket = bucket
        self._paths = paths or Paths()
        self._refresh_interval_s = refresh_interval_s
        self._context_naming = context_naming
        self._gcs = gcs or GCS(bucket)
        self._retries = max(1, retries)
        self._retry_base_delay_s = retry_base_delay_s

        # Cache state.
        self._lock = threading.Lock()
        self._assignments: dict[str, Any] | None = None
        self._last_read_at: float = 0.0
        # Round-robin cursor per template so multi-cluster templates spread
        # across their hosts on repeated calls.
        self._rr_cursors: dict[str, itertools.count] = {}

    # -- Public API --------------------------------------------------------

    def resolve(
        self,
        template: str,
        *,
        strategy: str = "round-robin",
        refresh: bool = False,
    ) -> ResolvedCluster:
        """Return the cluster hosting ``template``.

        ``strategy``:
          * ``"first"`` — deterministic sorted-name order (good for tests).
          * ``"round-robin"`` (default) — spread across hosting clusters over
            repeated calls. Keeps cache locality per-cluster while balancing
            across the fleet.
          * ``"hash"`` — deterministic hash(template) mod #hosts. Same
            template → same cluster, always. Use with image-affinity fleets
            for maximum cache locality.

        ``refresh`` forces a re-read of the assignments JSON even if the
        cache is still fresh.
        """
        matches = self._list_matches(template, refresh=refresh)
        if not matches:
            raise TemplateNotAssignedError(
                f"template {template!r} is not in the current assignment. "
                f"Check `fleetctl show-assignments` — either the operator "
                f"hasn't applied a spec that references it, or the underlying "
                f"SandboxTemplate CR is missing on every cluster."
            )

        chosen = self._choose(template, matches, strategy)
        return self._build_resolved(chosen)

    def list_matches(self, template: str, *, refresh: bool = False
                     ) -> list[ResolvedCluster]:
        """Return every cluster hosting ``template``. Useful when a caller
        wants to try more than one on failure, or to display all options."""
        matches = self._list_matches(template, refresh=refresh)
        return [self._build_resolved(m) for m in matches]

    def all_templates(self, *, refresh: bool = False) -> list[str]:
        """List every template name that appears in the current assignment."""
        assignments = self._read(refresh=refresh)
        names: set[str] = set()
        for entry in assignments.get("clusters", {}).values():
            for pool in entry.get("pools", []):
                names.add(pool["template"])
        return sorted(names)

    def snapshot(self, *, refresh: bool = False) -> dict[str, Any]:
        """Raw assignments JSON (mostly for debugging / CLI dumps)."""
        return dict(self._read(refresh=refresh))

    # -- Internal helpers --------------------------------------------------

    def _list_matches(self, template: str, *, refresh: bool) -> list[dict]:
        assignments = self._read(refresh=refresh)
        matches: list[dict] = []
        for cname, entry in assignments.get("clusters", {}).items():
            for pool in entry.get("pools", []):
                if pool.get("template") == template and pool.get("replicas", 0) > 0:
                    matches.append({
                        "cluster": cname,
                        "template": pool["template"],
                        "warmpool": pool["warmpool"],
                        "replicas": int(pool["replicas"]),
                        "image": pool.get("image"),
                        "generation": int(assignments.get("generation", 0)),
                    })
        # Deterministic base order — sort by cluster name — before any strategy applies.
        matches.sort(key=lambda m: m["cluster"])
        return matches

    def _choose(self, template: str, matches: list[dict], strategy: str) -> dict:
        if len(matches) == 1 or strategy == "first":
            return matches[0]
        if strategy == "round-robin":
            with self._lock:
                cursor = self._rr_cursors.setdefault(template, itertools.count())
                idx = next(cursor) % len(matches)
            return matches[idx]
        if strategy == "hash":
            digest = hashlib.md5(template.encode(), usedforsecurity=False).hexdigest()
            idx = int(digest, 16) % len(matches)
            return matches[idx]
        raise ValueError(
            f"unknown strategy {strategy!r}; expected one of first, round-robin, hash"
        )

    @property
    def context_naming(self) -> ContextNaming | None:
        """The configured cluster-name → kubeconfig-context mapper, if any.

        Public so callers holding a resolver (FleetSandboxClient) can name a
        context for a cluster they did not get from :meth:`resolve` without
        reaching into a private attribute. A property rather than a copy on
        the caller, so an injected resolver reports its own mapper.
        """
        return self._context_naming

    def _build_resolved(self, m: dict) -> ResolvedCluster:
        ctx = self._context_naming(m["cluster"]) if self._context_naming else None
        return ResolvedCluster(
            cluster=m["cluster"], template=m["template"], warmpool=m["warmpool"],
            replicas=m["replicas"], image=m["image"], generation=m["generation"],
            context_name=ctx,
        )

    def _read(self, *, refresh: bool) -> dict[str, Any]:
        now = time.time()
        with self._lock:
            fresh = (
                self._assignments is not None
                and (now - self._last_read_at) < self._refresh_interval_s
            )
        if fresh and not refresh:
            return self._assignments  # type: ignore[return-value]

        data = self._read_with_retry()
        with self._lock:
            self._assignments = data
            self._last_read_at = time.time()
        return data

    def _read_with_retry(self) -> dict[str, Any]:
        last_exc: Exception | None = None
        for attempt in range(self._retries):
            try:
                data = self._gcs.get_json(self._paths.assignments)
                if data is None:
                    raise AssignmentsMissingError(
                        f"gs://{self._bucket}/{self._paths.assignments} does not exist; "
                        f"run `fleetctl apply -f fleet-spec.yaml` first"
                    )
                return data
            except AssignmentsMissingError:
                # Not retriable — no amount of retrying creates the file.
                raise
            except Exception as e:  # noqa: BLE001 — transient GCS failures
                last_exc = e
                if attempt == self._retries - 1:
                    break
                delay = self._retry_base_delay_s * (2 ** attempt)
                log.warning(
                    "GCS read of %s failed (attempt %d/%d): %s — retrying in %.1fs",
                    self._paths.assignments, attempt + 1, self._retries, e, delay,
                )
                time.sleep(delay)
        assert last_exc is not None
        raise ResolverError(
            f"failed to read gs://{self._bucket}/{self._paths.assignments} "
            f"after {self._retries} attempts"
        ) from last_exc


def resolve_cluster(
    template: str,
    bucket: str,
    *,
    strategy: str = "round-robin",
    context_naming: ContextNaming | None = None,
    paths: Paths | None = None,
) -> ResolvedCluster:
    """One-shot convenience wrapper: build a resolver, resolve once, discard.

    Prefer :class:`ClusterResolver` for repeated resolutions so the assignments
    cache does its job. This helper exists for scripts and interactive use.
    """
    r = ClusterResolver(bucket, paths=paths, context_naming=context_naming)
    return r.resolve(template, strategy=strategy)


# ----------------------------------------------------------------------------
# FleetSandboxClient — the SDK-shaped facade
# ----------------------------------------------------------------------------

class FleetSandboxClient:
    """Multi-cluster wrapper over :class:`k8s_agent_sandbox.SandboxClient`.

    Given a fleet bucket and a way to name kubeconfig contexts (typically
    ``gke_context_naming(project, location)``), this object resolves a
    template to the correct cluster on every call and dispatches through a
    per-cluster ``SandboxClient`` that it constructs lazily.

    Example::

        from agent_sandbox_fleet import FleetSandboxClient, gke_context_naming

        client = FleetSandboxClient(
            bucket="agent-sandbox-fleet-vicente",
            context_naming=gke_context_naming(project="my-proj", location="us-central1-a"),
            namespace="multi-cluster-fleet",
        )

        # Caller writes zero multi-cluster code — resolver picks the cluster.
        sandbox = client.create_sandbox(template="sb-tmpl-django")
        # ...use it...
        client.delete_sandbox(sandbox.claim_name)

    Concurrency: safe to share across threads. Per-cluster client construction
    is serialised on an internal lock because ``load_kube_config`` mutates
    global state; concurrent ``create_sandbox`` calls afterwards run in
    parallel.
    """

    def __init__(
        self,
        bucket: str,
        *,
        context_naming: ContextNaming,
        namespace: str = "multi-cluster-fleet",
        resolve_strategy: str = "round-robin",
        refresh_interval_s: float = 60.0,
        paths: Paths | None = None,
        resolver: ClusterResolver | None = None,
        sandbox_client_factory: Callable[[], Any] | None = None,
        connection_pool_maxsize: int | None = None,
    ):
        """
        Parameters
        ----------
        bucket
            GCS bucket serving as the fleet hub.
        context_naming
            Function mapping cluster name → local kubeconfig context. Use
            :func:`gke_context_naming` on GKE or :func:`kind_context_naming`
            for the local dev demo.
        namespace
            Namespace where fleet-managed CRs live. Passed to every SDK call.
        resolve_strategy
            Default ``resolve()`` strategy. Individual calls may override.
        refresh_interval_s
            How stale the assignments cache may get before a resolve triggers
            a refresh.
        paths
            Optional custom :class:`Paths` (mostly for tests).
        resolver
            Optional pre-built :class:`ClusterResolver` (mostly for tests).
        sandbox_client_factory
            Optional factory for ``SandboxClient`` (mostly for tests). Called
            with no args; returns an object exposing ``create_sandbox`` and
            ``delete_sandbox`` methods.
        connection_pool_maxsize
            urllib3 connections to keep per cluster. **Set this to at least
            your concurrency.** ``create_sandbox`` holds a WATCH open for the
            whole call, so N concurrent claims need N live connections per
            cluster. Below that, urllib3 opens a fresh connection per request
            instead of reusing one, and the resulting connect storm gets
            refused by the control plane. Measured 2026-08-08 on a density
            fleet: concurrency 50 clean at 52 claims/s; concurrency 200 on
            the default pool produced ``ConnectTimeoutError`` against both
            apiservers. ``None`` leaves the client default.
        """
        self._namespace = namespace
        self._resolve_strategy = resolve_strategy
        self._resolver = resolver or ClusterResolver(
            bucket,
            paths=paths,
            refresh_interval_s=refresh_interval_s,
            context_naming=context_naming,
        )
        self._sandbox_client_factory = sandbox_client_factory
        self._connection_pool_maxsize = connection_pool_maxsize
        self._client_lock = threading.Lock()
        # cluster_name → SandboxClient
        self._clients: dict[str, Any] = {}
        # cluster_name → claim_name → cluster_name  (so delete_sandbox can find
        # the right per-cluster client from just the claim name, without
        # asking the caller to remember).
        self._claim_to_cluster: dict[str, str] = {}
        self._claim_lock = threading.Lock()

    # -- Public SDK-shaped surface ----------------------------------------

    def create_sandbox(
        self,
        template: str,
        *,
        namespace: str | None = None,
        strategy: str | None = None,
        sandbox_ready_timeout: int = 90,
        labels: dict[str, str] | None = None,
        **kwargs: Any,
    ) -> Any:
        """Resolve ``template`` to a cluster and create a sandbox there.

        Additional ``**kwargs`` are forwarded verbatim to the underlying
        ``SandboxClient.create_sandbox`` call — anything the SDK accepts
        (e.g. custom labels, timeouts) works here too.

        Returns whatever the underlying SDK returns. Records claim→cluster
        mapping so :meth:`delete_sandbox` can route the delete correctly
        without the caller having to remember which cluster served it.
        """
        resolved = self._resolver.resolve(
            template, strategy=strategy or self._resolve_strategy,
        )
        client = self._get_or_build_client(resolved)
        try:
            sandbox = client.create_sandbox(
                warmpool=resolved.warmpool,
                namespace=namespace or self._namespace,
                sandbox_ready_timeout=sandbox_ready_timeout,
                labels=labels or {},
                **kwargs,
            )
        except Exception:
            # If auth against the resolved cluster failed, drop the cached
            # client so a future call retries construction (e.g. after the
            # user re-runs `gcloud get-credentials`).
            self._drop_client(resolved.cluster)
            raise
        # Remember which cluster owns this claim so delete_sandbox routes.
        claim_name = _extract_claim_name(sandbox)
        if claim_name is not None:
            with self._claim_lock:
                self._claim_to_cluster[claim_name] = resolved.cluster
        return sandbox

    def delete_sandbox(
        self,
        claim_name: str,
        *,
        namespace: str | None = None,
        cluster: str | None = None,
    ) -> None:
        """Delete a claim. Uses the create-time claim→cluster mapping to
        route to the correct per-cluster SDK. If the claim wasn't created via
        this facade, pass ``cluster=`` explicitly.
        """
        with self._claim_lock:
            resolved_cluster = cluster or self._claim_to_cluster.get(claim_name)
        if not resolved_cluster:
            raise ResolverError(
                f"delete_sandbox: don't know which cluster owns claim "
                f"{claim_name!r}; pass cluster= explicitly, or create the "
                f"claim via this FleetSandboxClient so the mapping is recorded."
            )
        # Reuse the cached client if present; otherwise build via the
        # resolver's context_naming (still requires the caller to have the
        # kubeconfig context locally). Go through _get_or_build_client for
        # both cases: it does the cache lookup under _client_lock, and an
        # unlocked read here raced concurrent deletes against the dict every
        # other access on this class already guards.
        naming = self._resolver.context_naming
        ctx = naming(resolved_cluster) if naming else None
        client = self._get_or_build_client(ResolvedCluster(
            cluster=resolved_cluster, template="", warmpool="",
            replicas=0, image=None, generation=0, context_name=ctx,
        ))
        client.delete_sandbox(claim_name, namespace=namespace or self._namespace)
        with self._claim_lock:
            self._claim_to_cluster.pop(claim_name, None)

    # -- Introspection ----------------------------------------------------

    def resolve(self, template: str, *, strategy: str | None = None
                ) -> ResolvedCluster:
        """Expose the underlying resolver directly."""
        return self._resolver.resolve(
            template, strategy=strategy or self._resolve_strategy,
        )

    def known_clusters(self) -> list[str]:
        """Names of clusters we've already built a client for."""
        with self._client_lock:
            return sorted(self._clients)

    # -- Internal helpers -------------------------------------------------

    def _get_or_build_client(self, resolved: ResolvedCluster) -> Any:
        with self._client_lock:
            existing = self._clients.get(resolved.cluster)
            if existing is not None:
                return existing
            ctx = resolved.context_name
            if ctx is None:
                raise ClusterUnavailableError(
                    f"cluster {resolved.cluster!r}: no context_naming set on "
                    f"the resolver. Construct FleetSandboxClient with "
                    f"context_naming=gke_context_naming(project, location) or "
                    f"kind_context_naming()."
                )
            client = self._build_client(ctx)
            self._clients[resolved.cluster] = client
            return client

    def _build_client(self, context_name: str) -> Any:
        """Load the given kubeconfig context and construct a SandboxClient
        that is PINNED to that context.

        Why the manual api-rebind at the end:
        The SDK's K8sHelper.__init__ calls ``config.load_kube_config()`` with
        no context arg, which resets the process-wide default to whatever
        ``current-context`` says in ~/.kube/config. Then it instantiates
        ``CustomObjectsApi()`` / ``CoreV1Api()`` with no explicit configuration
        — so those API objects capture whatever the LAST kubectl / gcloud
        get-credentials made the current context. Result: every cached
        SandboxClient across the fleet routes to the same (last-primed)
        cluster, even when we thought we were building a per-cluster client.
        (Diagnosed 2026-07-17 during the XS stress-test — 2/3 clusters got
        100% "SandboxWarmPool not found" while the last-primed cluster got
        100% success.)

        The workaround: after building SandboxClient(), replace its internal
        API instances with fresh ones bound to an explicit per-context
        Configuration. This bypasses global state entirely.

        Lazy-imports the SDK and kubernetes client so the fleet package
        stays importable without them (matters for unit tests and the
        planner-only case).
        """
        if self._sandbox_client_factory is not None:
            # Test hook: build via injected factory. The factory owns any
            # kubeconfig loading it needs.
            return self._sandbox_client_factory()
        try:
            from kubernetes import client as k8s_client, config as k8s_config  # local import
        except ImportError as e:
            raise ClusterUnavailableError(
                "kubernetes client not installed; `pip install kubernetes`"
            ) from e
        try:
            from k8s_agent_sandbox import SandboxClient  # local import
        except ImportError as e:
            raise ClusterUnavailableError(
                "k8s-agent-sandbox SDK not installed; "
                "`pip install k8s-agent-sandbox`"
            ) from e

        # 1. Build a fresh, isolated Configuration for this context. Passing
        #    client_configuration= to load_kube_config directs the loader to
        #    populate THIS object instead of the global default.
        cfg = k8s_client.Configuration()
        try:
            k8s_config.load_kube_config(
                context=context_name, client_configuration=cfg,
            )
        except k8s_config.ConfigException as e:
            raise ClusterUnavailableError(
                f"kubeconfig context {context_name!r} not found. Run "
                f"`gcloud container clusters get-credentials <cluster> "
                f"--zone=<zone> --project=<project>` first."
            ) from e

        # 1b. Size the connection pool BEFORE any ApiClient is built from cfg.
        #     create_sandbox() holds a watch open for the duration of the
        #     call, so concurrency N needs N live connections to this cluster.
        #     Leaving the default here is what produced ConnectTimeoutError
        #     at concurrency 200 (2026-08-08) — urllib3 stops reusing and the
        #     control plane refuses the resulting connect storm.
        if self._connection_pool_maxsize is not None:
            cfg.connection_pool_maxsize = self._connection_pool_maxsize

        # 2. Also load into the global default so SandboxClient's K8sHelper
        #    __init__ (which calls plain load_kube_config()) doesn't error on
        #    "no current context". The global default here is transient — we
        #    overwrite the internal apis on the very next line.
        try:
            k8s_config.load_kube_config(context=context_name)
        except k8s_config.ConfigException:
            # Swallow — we've already got a valid cfg for this cluster; the
            # SDK's internal load is only used for the discarded default apis.
            pass

        sandbox_client = SandboxClient()

        # 3. Rebind the SDK's internal API objects to an ApiClient tied to
        #    our explicit per-context Configuration. This is the load-bearing
        #    step — without it, every SandboxClient across the fleet would
        #    route to the same cluster (see docstring).
        api_client = k8s_client.ApiClient(configuration=cfg)
        sandbox_client.k8s_helper.custom_objects_api = k8s_client.CustomObjectsApi(api_client)
        sandbox_client.k8s_helper.core_v1_api = k8s_client.CoreV1Api(api_client)

        log.debug("built pinned SandboxClient for context=%s host=%s",
                  context_name, cfg.host)
        return sandbox_client

    def _drop_client(self, cluster: str) -> None:
        with self._client_lock:
            self._clients.pop(cluster, None)


def _extract_claim_name(sandbox: Any) -> str | None:
    """SDK returns different shapes across versions; pick the claim name
    field defensively."""
    for attr in ("claim_name", "name", "sandbox_name"):
        val = getattr(sandbox, attr, None)
        if val:
            return str(val)
    return None
