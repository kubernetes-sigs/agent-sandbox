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

import copy
import logging
import math
import time
from itertools import chain
from datetime import UTC, datetime
from collections.abc import Callable, Iterator
from typing import Any, List, cast
from kubernetes import client, config, watch
import urllib3.exceptions
from .exceptions import SandboxClaimFailedError, SandboxMetadataError, SandboxNotFoundError, SandboxTemplateNotFoundError, SandboxWarmPoolNotFoundError
from .models import AdditionalPodMetadata
from .utils import (
    construct_sandbox_claim_env_spec,
    is_valid_gateway_hostname,
    is_valid_ip,
    select_pod_ip,
    validate_pool_size,
)
from .constants import (
    CLAIM_API_GROUP,
    CLAIM_API_VERSION,
    CLAIM_LIST_PAGE_SIZE,
    CLAIM_PLURAL_NAME,
    TEMPLATE_PLURAL_NAME,
    TERMINAL_CLAIM_READY_REASONS,
    WARMPOOL_PLURAL_NAME,
    CLIENT_REQUEST_TIME_ANNOTATION,
    GATEWAY_API_GROUP,
    GATEWAY_API_VERSION,
    GATEWAY_PLURAL,
    SANDBOX_API_GROUP,
    SANDBOX_API_VERSION,
    SANDBOX_PLURAL_NAME,
    CREATED_BY_LABEL,
)

logger = logging.getLogger(__name__)


class _UnretriedApiClient(client.ApiClient):
    """Wraps ``source`` with its own ``rest_client`` where ``configuration.retries`` is ``False``,
    so batch Lease and custom-object callers run their own deadline- and conflict-aware retry logic
    without disabling retries on ``source``.

    ``ApiClient.__init__`` builds ``rest_client`` from ``configuration.retries`` at construction
    time, so those callers need a separate ``ApiClient`` rather than mutating ``source``. Copying
    ``default_headers`` and ``cookie`` once at construction would miss later ``set_default_header``
    calls or ``cookie`` updates on ``source``, so properties read them from ``source`` on each
    request.
    """

    def __init__(self, source: client.ApiClient) -> None:
        # ApiClient.__init__ assigns default_headers and cookie before __init__ sets _source.
        self._source: client.ApiClient | None = None
        self._default_headers: dict[Any, Any] = {}
        self._cookie: Any = None
        # A shallow copy shares the credential dicts, so a token the caller rotates in place on the
        # injected configuration also reaches batch requests, while only the copy disables retries.
        configuration = copy.copy(source.configuration)
        configuration.retries = False
        super().__init__(configuration=configuration)
        self._source = source

    @property
    def default_headers(self) -> dict[Any, Any]:
        return self._source.default_headers if self._source is not None else self._default_headers

    @default_headers.setter
    def default_headers(self, value: dict[Any, Any]) -> None:
        self._default_headers = value

    @property
    def cookie(self) -> Any:
        return self._source.cookie if self._source is not None else self._cookie

    @cookie.setter
    def cookie(self, value: Any) -> None:
        self._cookie = value


def _api_client_without_retries(api_client: client.ApiClient | None) -> client.ApiClient:
    """Returns an ``ApiClient`` like ``api_client`` (or the default one) whose requests urllib3
    doesn't retry, so batch Lease and custom-object callers run their own deadline- and
    conflict-aware retry logic.

    urllib3 retries a timed-out request with a fresh timeout and sleeps on Retry-After headers by
    default, which would stretch a Lease renewal or batch custom-object request past its deadline or
    the Lease's expiry inside one SDK call, and a retried Lease write that already landed comes back
    as a conflict. The async helper needs no counterpart, since aiohttp doesn't retry requests.
    """
    if api_client is not None:
        return _UnretriedApiClient(api_client)
    configuration = client.Configuration.get_default_copy()
    configuration.retries = False
    return client.ApiClient(configuration=configuration)


def _claim_list_pages(
    list_page: Callable[..., Any],
    namespace: str,
    label_selector: str,
    _request_timeout: float | tuple[float, float] | None,
    *,
    deadline: float | None = None,
) -> Iterator[tuple[dict[str, Any], bool]]:
    """Yields ``(page, first)`` pairs for one paginated ``SandboxClaim`` list.

    An unpaginated ``SandboxClaim`` list fetches every claim of a batch in one HTTP response,
    holding the full raw JSON body and decoded text alongside the parsed dicts in driver memory
    during ``claim_batch``, ``get_batch``, ``410`` watch re-lists, and release counts.
    ``_claim_list_pages`` fetches the list in ``CLAIM_LIST_PAGE_SIZE`` chunks so driver memory
    holds only one page's response buffer at a time while keeping a single consistent snapshot
    at the first page's ``resourceVersion``.

    Yielding ``(page, first)`` tells the caller when the list starts (or restarts after a ``410``
    continue-token expiry) so the caller discards anything it gathered from earlier pages.
    """
    continue_token: str | None = None
    restarted = False
    while True:
        page_timeout = _request_timeout
        if deadline is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("SandboxClaim list exceeded its deadline")
            if page_timeout is None:
                page_timeout = remaining
            elif isinstance(page_timeout, (int, float)):
                page_timeout = min(float(page_timeout), remaining)
            else:
                page_timeout = (min(page_timeout[0], remaining), min(page_timeout[1], remaining))
        try:
            page = cast(dict[str, Any], list_page(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                label_selector=label_selector,
                limit=CLAIM_LIST_PAGE_SIZE,
                _continue=continue_token,
                _request_timeout=page_timeout,
            ))
        except client.ApiException as e:
            # A continue token expires after a few minutes, and a 410 isn't transient. Restarting the
            # list once without the token keeps its pages one consistent snapshot, which a watch
            # started at the list's resourceVersion relies on.
            if e.status != 410 or continue_token is None or restarted:
                raise
            restarted = True
            continue_token = None
            continue
        yield page, continue_token is None
        # A page may be empty while more follow, so only the continue token ends the list.
        continue_token = (page.get("metadata") or {}).get("continue") or None
        # Drop the generator frame's reference before fetching the next page so driver memory
        # never holds two pages at once.
        del page
        if continue_token is None:
            return


class K8sHelper:
    """Helper class for Kubernetes API interactions."""

    def __init__(
        self,
        api_client: client.ApiClient | None = None,
        pool_size: int | None = None,
        *,
        kube_config_temp_dir: str | None = None,
    ) -> None:
        """When ``api_client`` is provided it is used directly; otherwise the underlying Kubernetes client
        config is loaded (in-cluster, then ``KUBECONFIG``, falling back to ``~/.kube/config`` if unset).
        ``pool_size`` sets the loaded client's connection pool size and can't be combined with
        ``api_client``.

        ``kube_config_temp_dir`` is where inline kubeconfig cert data is extracted to files. Pass a directory
        the caller removes when the library's own atexit cleanup of those files cannot be registered."""
        validate_pool_size(pool_size, api_client)
        # Kept so kubectl subprocesses can target the same cluster.
        self.injected_api_client = api_client
        if api_client is None:
            try:
                config.load_incluster_config()
            except config.ConfigException:
                config.load_kube_config(temp_file_path=kube_config_temp_dir)
            if pool_size is not None:
                configuration = client.Configuration.get_default_copy()
                configuration.connection_pool_maxsize = pool_size
                api_client = client.ApiClient(configuration=configuration)
        self.custom_objects_api = client.CustomObjectsApi(api_client)
        unretried_api_client = _api_client_without_retries(api_client)
        self._batch_custom_objects_api = client.CustomObjectsApi(unretried_api_client)
        self.core_v1_api = client.CoreV1Api(api_client)
        self.coordination_v1_api = client.CoordinationV1Api(unretried_api_client)

    def api_pool_size(self) -> int | None:
        """Returns the size of the Kubernetes API connection pool that creates and the watch use, or
        ``None`` if it is unbounded."""
        # urllib3 backs its pool with LifoQueue(maxsize), which treats 0 as no limit.
        maxsize = cast(
            "int | None", self.custom_objects_api.api_client.configuration.connection_pool_maxsize
        )
        return maxsize or None

    def create_sandbox_claim(
        self,
        name: str,
        warmpool: str,
        namespace: str,
        annotations: dict[str, str] | None = None,
        labels: dict[str, str] | None = None,
        lifecycle: dict[str, Any] | None = None,
        volume_claim_templates: list[dict[str, Any]] | None = None,
        pod_metadata: AdditionalPodMetadata | dict[str, Any] | None = None,
        env: dict[str, str] | None = None,
        owner_references: list[dict[str, str]] | None = None,
        *,
        log_level: int = logging.INFO,
        _request_timeout: float | tuple[float, float] | None = None,
        batch: bool = False,
    ) -> dict[str, Any]:
        """Creates a SandboxClaim custom resource.

        Args:
            pod_metadata: Optional ``AdditionalPodMetadata`` (or an equivalent
                ``dict``, validated via ``model_validate``) serialized as
                ``spec.additionalPodMetadata`` so the labels and annotations
                propagate onto the running Sandbox Pod (as opposed to
                ``labels``, which only land on the SandboxClaim object).
            owner_references: Optional ``metadata.ownerReferences`` for the claim, so the
                garbage collector deletes it once its owner is gone.
            log_level: Log level for the logger during this function call.
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
            batch: If ``True``, route the request through the unretried batch
                custom-object client so connection errors, read errors, and
                ``Retry-After`` responses surface after one attempt.
        """
        updated_annotations = dict(annotations) if annotations else {}
        if CLIENT_REQUEST_TIME_ANNOTATION not in updated_annotations:
            updated_annotations[CLIENT_REQUEST_TIME_ANNOTATION] = datetime.now(UTC).isoformat()

        metadata: dict[str, Any] = {
            "name": name,
            "annotations": updated_annotations,
            "labels": {
                **(labels or {}),
                CREATED_BY_LABEL: "python-client",
            }
        }
        if owner_references:
            metadata["ownerReferences"] = owner_references

        spec: dict[str, Any] = {
            "warmPoolRef": {
                "name": warmpool
            }
        }
        if lifecycle:
            spec["lifecycle"] = lifecycle
        if volume_claim_templates:
            spec["volumeClaimTemplates"] = volume_claim_templates
        if pod_metadata:
            if isinstance(pod_metadata, dict):
                pod_metadata = AdditionalPodMetadata.model_validate(pod_metadata)
            spec["additionalPodMetadata"] = pod_metadata.model_dump(exclude_none=True)
        env_spec = construct_sandbox_claim_env_spec(env)
        if env_spec:
            spec["env"] = [
                env_var.model_dump(by_alias=True, exclude_none=True)
                for env_var in env_spec
            ]

        manifest = {
            "apiVersion": f"{CLAIM_API_GROUP}/{CLAIM_API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": metadata,
            "spec": spec,
        }
        # Route per-claim batch creates through _batch_custom_objects_api so urllib3 does not
        # retry past SandboxBatch._create_one's deadline or Lease budget.
        api = self._batch_custom_objects_api if batch else self.custom_objects_api
        logger.log(log_level, f"Creating SandboxClaim '{name}' in namespace '{namespace}' using warm pool '{warmpool}'...")
        return cast(dict[str, Any], api.create_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=namespace,
            plural=CLAIM_PLURAL_NAME,
            body=manifest,
            _request_timeout=_request_timeout,
        ))

    def resolve_sandbox_name(self, claim_name: str, namespace: str, timeout: int, resource_version: str | None = None) -> str:
        """Resolves the actual Sandbox name from the SandboxClaim status.
        With warm pool adoption, the sandbox name may differ from the claim
        name. This method watches the SandboxClaim until the sandbox name
        appears in the claim's status, then returns it.

        Args:
            resource_version: Optional resourceVersion to start the watch
                from (e.g. ``metadata.resourceVersion`` of the create
                response). Defaults to ``"0"`` — see ``_watch_claim``.
        """
        return self._watch_claim(claim_name, namespace, timeout, require_ready=False,
                                 resource_version=resource_version)

    def wait_for_claim_ready(
        self, claim_name: str, namespace: str, timeout: int, resource_version: str | None = None,
        *, expected_uid: str | None = None, initial_claim: dict[str, Any] | None = None,
        require_current_generation: bool = False,
    ) -> str:
        """Watches the SandboxClaim until it is bound to a sandbox AND its
        Ready condition is True, then returns the sandbox name.

        This is the lowest-latency ready-wait: the claim controller writes
        ``status.sandbox.name``, ``status.sandbox.podIPs`` and the forwarded
        Ready condition in a single status update on warm-pool adoption, so a
        single watch on the claim observes readiness without a second watch
        on the Sandbox resource (the claim's Ready condition is a direct
        forward of the Sandbox's Ready condition).

        Args:
            resource_version: Optional resourceVersion to start the watch
                from (e.g. ``metadata.resourceVersion`` of the create
                response). Defaults to ``"0"`` — see ``_watch_claim``.
            expected_uid: If provided, reject events for a same-name replacement.
            initial_claim: Optional create/GET response to evaluate before
                waiting for future watch events.
            require_current_generation: If True, also require the Ready condition's
                observedGeneration to equal metadata.generation in each snapshot.
                Missing generation fields remain pending. Defaults to False.
        """
        return self._watch_claim(claim_name, namespace, timeout, require_ready=True,
                                 resource_version=resource_version,
                                 expected_uid=expected_uid, initial_claim=initial_claim,
                                 require_current_generation=require_current_generation)

    def _watch_claim(self, claim_name: str, namespace: str, timeout: int, require_ready: bool,
                     resource_version: str | None = None,
                     *, expected_uid: str | None = None, initial_claim: dict[str, Any] | None = None,
                     require_current_generation: bool = False) -> str:
        """Shared SandboxClaim watch loop.

        Returns the sandbox name once ``status.sandbox.name`` is populated;
        when ``require_ready`` is set, additionally waits until the claim's
        Ready condition is True.

        The watch always starts from an explicit resourceVersion — the
        claim's own (from the create response) when the caller has it, else
        ``"0"`` — so the apiserver serves it from the watch cache. A watch
        with UNSET resourceVersion forces a quorum etcd read to establish
        initial state on every wait, which at high claim rates is pure
        apiserver/etcd load on the latency path. If the supplied version has
        been compacted away (410 Gone), the watch transparently restarts from
        ``"0"`` (current state from the watch cache).
        """
        wait_target = "claim readiness" if require_ready else "sandbox name"
        # Existing tests assert on the exact resolve-path message.
        deleted_msg = (f"SandboxClaim '{claim_name}' was deleted while waiting for claim readiness"
                       if require_ready else
                       f"SandboxClaim '{claim_name}' was deleted while resolving sandbox name")
        deadline = time.monotonic() + timeout
        rv = resource_version or "0"
        logger.info(f"Watching claim '{claim_name}' for {wait_target} (from resourceVersion={rv})...")
        while True:
            # timeoutSeconds is integral; preserve a positive fractional budget.
            remaining = math.ceil(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(
                    f"Could not resolve {wait_target} from claim "
                    f"'{claim_name}' within {timeout} seconds.")
            w = watch.Watch()
            try:
                event_iter = w.stream(
                    func=self.custom_objects_api.list_namespaced_custom_object,
                    namespace=namespace,
                    group=CLAIM_API_GROUP,
                    version=CLAIM_API_VERSION,
                    plural=CLAIM_PLURAL_NAME,
                    field_selector=f"metadata.name={claim_name}",
                    resource_version=rv,
                    timeout_seconds=remaining
                )
                # An already-ready GET may never produce another watch event.
                initial_events = [] if initial_claim is None else [
                    {"type": "ADDED", "object": initial_claim}
                ]
                initial_claim = None
                for event in chain(initial_events, event_iter):
                    if event is None:
                        continue
                    if event["type"] == "DELETED":
                        raise SandboxMetadataError(deleted_msg)
                    if event["type"] in ["ADDED", "MODIFIED"]:
                        claim_object = event['object']
                        metadata = claim_object.get("metadata") or {}
                        if expected_uid is not None and metadata.get("uid") != expected_uid:
                            raise SandboxNotFoundError(
                                f"SandboxClaim '{claim_name}' was replaced while waiting for readiness."
                            )
                        if expected_uid is not None and metadata.get("deletionTimestamp"):
                            raise SandboxNotFoundError(f"SandboxClaim '{claim_name}' is terminating.")
                        # Track the last-seen resourceVersion so a stream
                        # restart resumes instead of replaying history.
                        seen_rv = (claim_object.get('metadata') or {}).get('resourceVersion')
                        if seen_rv:
                            rv = seen_rv
                        status = claim_object.get('status') or {}

                        ready = False
                        for cond in status.get('conditions', []):
                            if (
                                cond.get('type') == 'Ready'
                                and cond.get('status') == 'False'
                                and cond.get('reason') == 'TemplateNotFound'
                            ):
                                raise SandboxTemplateNotFoundError(
                                    f"SandboxTemplate requested does not exist: {cond.get('message', 'Template not found')}"
                                )
                            elif cond.get('reason') == 'WarmPoolNotFound':
                                raise SandboxWarmPoolNotFoundError(
                                    f"SandboxWarmPool requested does not exist: {cond.get('message', 'WarmPool not found')}"
                                )
                            elif (
                                cond.get('type') == 'Ready'
                                and cond.get('status') == 'False'
                                and cond.get('reason') in TERMINAL_CLAIM_READY_REASONS
                            ):
                                # The controller reported a failure it will not
                                # retry; waiting out the timeout cannot succeed.
                                raise SandboxClaimFailedError(
                                    f"SandboxClaim '{claim_name}' failed with terminal reason "
                                    f"{cond.get('reason')}: {cond.get('message', '')}"
                                )
                            if (cond.get('type') == 'Ready' and cond.get('status') == 'True'
                                    and (not require_current_generation
                                         or (metadata.get('generation') is not None
                                             and cond.get('observedGeneration') == metadata['generation']))):
                                ready = True

                        sandbox_status = status.get('sandbox', {})
                        # Support both 'name' (standard) and 'Name' (legacy, before CRD rename in #440)
                        name = sandbox_status.get('name', '') or sandbox_status.get('Name', '')
                        if name and (ready or not require_ready):
                            logger.info(
                                f"Resolved sandbox name '{name}' from claim status"
                                + (" (claim Ready)" if ready else ""))
                            return cast(str, name)
            except client.ApiException as e:
                if e.status == 410:
                    # The requested resourceVersion was compacted away:
                    # restart from the watch cache's current state.
                    logger.info(
                        f"Watch on claim '{claim_name}' expired (410 Gone at resourceVersion={rv}); "
                        "restarting from current state")
                    rv = "0"
                    continue
                raise
            except (
                urllib3.exceptions.ProtocolError,
                urllib3.exceptions.ReadTimeoutError,
                ConnectionError,
            ) as e:
                logger.warning(
                    f"Watch on claim '{claim_name}' disconnected ({type(e).__name__}: {e}); "
                    "reconnecting..."
                )
                time.sleep(min(0.5, max(0.0, deadline - time.monotonic())))
                continue
            finally:
                w.stop()

    def wait_for_sandbox_ready(self, name: str, namespace: str, timeout: int) -> str | None:
        """Waits for the Sandbox custom resource to have a 'Ready' status.

        Returns the selected pod IP from the sandbox status when ready, or None if
        no valid IP can be selected.
        """
        deadline = time.monotonic() + timeout
        logger.info(f"Watching for Sandbox {name} to become ready...")
        while True:
            remaining = math.ceil(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Sandbox {name} did not become ready within {timeout} seconds.")
            w = watch.Watch()
            try:
                for event in w.stream(
                    func=self.custom_objects_api.list_namespaced_custom_object,
                    namespace=namespace,
                    group=SANDBOX_API_GROUP,
                    version=SANDBOX_API_VERSION,
                    plural=SANDBOX_PLURAL_NAME,
                    field_selector=f"metadata.name={name}",
                    timeout_seconds=remaining
                ):
                    if event is None:
                        continue
                    if event["type"] in ["ADDED", "MODIFIED"]:
                        sandbox_object = event['object']
                        status = sandbox_object.get('status') or {}
                        conditions = status.get('conditions', [])
                        for cond in conditions:
                            if cond.get('type') == 'Ready' and cond.get('status') == 'True':
                                logger.info(f"Sandbox {name} is ready.")
                                w.stop()
                                pod_ips = status.get('podIPs', [])
                                return select_pod_ip(pod_ips)
                    elif event["type"] == "DELETED":
                        logger.error(f"Sandbox {name} was deleted before becoming ready.")
                        w.stop()
                        raise SandboxNotFoundError(f"Sandbox {name} was deleted before becoming ready.")
            except (
                urllib3.exceptions.ProtocolError,
                urllib3.exceptions.ReadTimeoutError,
                ConnectionError,
            ) as e:
                logger.warning(
                    f"Watch for Sandbox '{name}' disconnected ({type(e).__name__}: {e}); "
                    "reconnecting..."
                )
                time.sleep(min(0.5, max(0.0, deadline - time.monotonic())))
                continue

    def delete_sandbox_claim(
        self, name: str, namespace: str, _request_timeout: float | tuple[float, float] | None = None,
        *, expected_uid: str | None = None, log_level: int = logging.INFO, batch: bool = False,
    ) -> None:
        """Deletes a SandboxClaim custom resource.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3-based request.
            expected_uid: If provided, delete only the Claim with this UID.
            log_level: Log level for the logger during this function call.
            batch: If ``True``, route the request through the unretried batch
                custom-object client so connection errors, read errors, and
                ``Retry-After`` responses surface after one attempt.
        """
        delete_kwargs: dict[str, Any] = {}
        if expected_uid is not None:
            delete_kwargs["body"] = client.V1DeleteOptions(
                preconditions=client.V1Preconditions(uid=expected_uid)
            )
        # Route per-claim batch deletes through _batch_custom_objects_api so urllib3 does not
        # retry past a batch Lease deadline while non-batch callers (including timeout-bounded
        # atexit cleanup) keep default urllib3 retries.
        api = self._batch_custom_objects_api if batch else self.custom_objects_api
        try:
            api.delete_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                name=name,
                _request_timeout=_request_timeout,
                **delete_kwargs,
            )
            logger.log(log_level, f"Terminated SandboxClaim: {name}")
        except client.ApiException as e:
            if e.status != 404:
                logger.error(f"Error terminating SandboxClaim {name}: {e}")
                raise

    def get_sandbox(self, name: str, namespace: str) -> dict[str, Any] | None:
        """Gets a Sandbox custom resource."""
        try:
            # The generated client types custom-object responses as ``object``
            # (kubernetes >= 37); they are the decoded JSON dict at runtime.
            return cast(dict[str, Any], self.custom_objects_api.get_namespaced_custom_object(
                group=SANDBOX_API_GROUP,
                version=SANDBOX_API_VERSION,
                namespace=namespace,
                plural=SANDBOX_PLURAL_NAME,
                name=name,
            ))
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    def patch_sandbox_claim(self, name: str, namespace: str, body: dict[str, Any]) -> dict[str, Any]:
        """Patches a SandboxClaim custom resource."""
        return cast(dict[str, Any], self.custom_objects_api.patch_namespaced_custom_object(
            group=CLAIM_API_GROUP,
            version=CLAIM_API_VERSION,
            namespace=namespace,
            plural=CLAIM_PLURAL_NAME,
            name=name,
            body=body
        ))

    def get_sandbox_claim(self, name: str, namespace: str) -> dict[str, Any] | None:
        """Gets a SandboxClaim custom resource (or ``None`` if it doesn't exist)."""
        try:
            return cast(dict[str, Any], self.custom_objects_api.get_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                name=name
            ))
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    def list_sandbox_claims(self, namespace: str, label_selector: str | None = None) -> List[str]:
        """Lists all SandboxClaim custom resources in a namespace.

        Args:
            namespace: Kubernetes namespace to list claims in.
            label_selector: Optional Kubernetes label selector string
                (e.g. ``"app=myapp,env=prod"``). When set, only claims
                matching the selector are returned.
        """
        try:
            kwargs: dict[str, Any] = dict(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
            )
            if label_selector is not None:
                kwargs["label_selector"] = label_selector
            response = cast(dict[str, Any], self.custom_objects_api.list_namespaced_custom_object(**kwargs))
            return [
                item.get("metadata", {}).get("name") 
                for item in response.get("items", []) 
                if item.get("metadata", {}).get("name")
            ]
        except client.ApiException as e:
            logger.error(f"Error listing sandbox claims in namespace {namespace}: {e}")
            raise

    def wait_for_gateway_ip(self, gateway_name: str, namespace: str, timeout: int) -> str:
        """Waits for the Gateway to be assigned an external IP."""
        deadline = time.monotonic() + timeout
        logger.info(f"Waiting for Gateway '{gateway_name}' in namespace '{namespace}'...")
        while True:
            remaining = math.ceil(deadline - time.monotonic())
            if remaining <= 0:
                raise TimeoutError(f"Gateway '{gateway_name}' did not get an IP.")
            w = watch.Watch()
            for event in w.stream(
                func=self.custom_objects_api.list_namespaced_custom_object,
                namespace=namespace,
                group=GATEWAY_API_GROUP,
                version=GATEWAY_API_VERSION,
                plural=GATEWAY_PLURAL,
                field_selector=f"metadata.name={gateway_name}",
                timeout_seconds=remaining,
            ):
                if event is None:
                    continue
                if event["type"] in ["ADDED", "MODIFIED"]:
                    gateway_object = event['object']
                    status = gateway_object.get('status') or {}
                    addresses = status.get('addresses', [])
                    for address in addresses:
                        if not isinstance(address, dict):
                            continue
                        ip_address = address.get('value')
                        if not ip_address:
                            continue
                        
                        if not is_valid_ip(ip_address) and not is_valid_gateway_hostname(ip_address):
                            logger.warning(
                                "Gateway address rejected because %r is neither a valid IP address nor a valid gateway hostname.",
                                ip_address,
                            )
                            continue
                        
                        logger.info(f"Gateway ready. IP: {ip_address}")
                        w.stop()
                        return cast(str, ip_address)

    def list_sandbox_claim_objects(
        self,
        namespace: str,
        label_selector: str,
        _request_timeout: float | tuple[float, float] | None = None,
        *,
        deadline: float | None = None,
    ) -> tuple[list[dict[str, Any]], str]:
        """Lists full SandboxClaim objects matching a label selector. Also returns the list's
        resourceVersion, so a subsequent watch does not miss any events.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request of each page.
            deadline: Optional ``time.monotonic()`` deadline across the whole paginated list;
                caps each page's ``_request_timeout`` to the remaining budget and raises
                ``TimeoutError`` once the deadline passes.
        """
        items: list[dict[str, Any]] = []
        resource_version = "0"
        for page, first in _claim_list_pages(
            self._batch_custom_objects_api.list_namespaced_custom_object,
            namespace,
            label_selector,
            _request_timeout,
            deadline=deadline,
        ):
            if first:
                items = []
                resource_version = (page.get("metadata") or {}).get("resourceVersion", "0")
            page_items = page.get("items", [])
            # Nothing in the SDK reads managedFields, so drop it only in memory from each parsed
            # dict to lower driver memory without modifying the live SandboxClaim in the cluster.
            for claim in page_items:
                (claim.get("metadata") or {}).pop("managedFields", None)
            items.extend(page_items)
        return items, resource_version

    def count_live_sandbox_claims(
        self,
        namespace: str,
        label_selector: str,
        _request_timeout: float | tuple[float, float] | None = None,
    ) -> int:
        """Counts matching ``SandboxClaim`` objects without ``metadata.deletionTimestamp``,
        holding one page of the list at a time.

        Between ``deletecollection`` rounds, ``release()`` and ``release_group()`` run a
        consistent read only to check whether any non-terminating claims still need deletion and
        whether the round made progress. Calling ``list_sandbox_claim_objects`` would materialize
        every surviving claim dict in driver memory just to count them (especially after a
        ``deletecollection`` timeout on a large batch), whereas ``count_live_sandbox_claims``
        folds one page at a time into an ``int`` and drops each page before advancing the
        generator.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request of each page.
        """
        live = 0
        for page, first in _claim_list_pages(
            self._batch_custom_objects_api.list_namespaced_custom_object, namespace, label_selector, _request_timeout
        ):
            if first:
                live = 0
            live += sum(1 for c in page.get("items", []) if not (c.get("metadata") or {}).get("deletionTimestamp"))
            # Drop this frame's reference before advancing the generator to fetch the next page.
            del page
        return live

    def watch_sandbox_claims(
        self, namespace: str, label_selector: str, resource_version: str, timeout_seconds: int,
        _request_timeout: float | tuple[float, float] | None = None
    ) -> Iterator[dict[str, Any]]:
        """Uses a single watch starting at ``resource_version`` to yield raw watch events for multiple
        SandboxClaims matching a label selector.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
        """
        w = watch.Watch()
        try:
            for event in w.stream(
                func=self._batch_custom_objects_api.list_namespaced_custom_object,
                namespace=namespace,
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                plural=CLAIM_PLURAL_NAME,
                label_selector=label_selector,
                resource_version=resource_version,
                timeout_seconds=timeout_seconds,
                allow_watch_bookmarks=True,
                _request_timeout=_request_timeout,
            ):
                if event is not None:
                    yield event
        finally:
            w.stop()

    def read_batch_lease(
        self, name: str, namespace: str, _request_timeout: float | tuple[float, float] | None = None
    ) -> client.V1Lease | None:
        """Reads a batch Lease, or ``None`` if it doesn't exist.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
        """
        try:
            return self.coordination_v1_api.read_namespaced_lease(
                name, namespace, _request_timeout=_request_timeout
            )
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise

    def replace_batch_lease(
        self,
        name: str,
        namespace: str,
        body: client.V1Lease,
        _request_timeout: float | tuple[float, float] | None = None,
    ) -> client.V1Lease:
        """Replaces a batch Lease, then returns the updated object.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
        """
        # Uses replace (PUT) over PATCH to have the apiserver reject stale resourceVersion updates
        # to handle races and ensure two clients cannot simultaneously hold the same batch lease.
        return self.coordination_v1_api.replace_namespaced_lease(
            name, namespace, body, _request_timeout=_request_timeout
        )

    def create_batch_lease(
        self, namespace: str, body: client.V1Lease, _request_timeout: float | tuple[float, float] | None = None
    ) -> client.V1Lease:
        """Creates a batch Lease, then returns the created object. Raises a 409 ``ApiException`` if the Lease exists.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
        """
        return self.coordination_v1_api.create_namespaced_lease(
            namespace, body, _request_timeout=_request_timeout
        )

    def delete_batch_lease(
        self, name: str, namespace: str, _request_timeout: float | tuple[float, float] | None = None,
        *, resource_version: str | None = None,
    ) -> None:
        """Deletes a batch Lease. A Lease that doesn't exist is treated as deleted.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
            resource_version: If provided, delete the Lease only at this resourceVersion. A Lease
                someone has written since is left in place and also treated as deleted.

        Raises:
            ValueError: If ``resource_version`` is an empty string.
        """
        if resource_version is not None and not resource_version:
            raise ValueError("resource_version must not be empty")
        delete_kwargs: dict[str, Any] = {}
        if resource_version is not None:
            delete_kwargs["body"] = client.V1DeleteOptions(
                preconditions=client.V1Preconditions(resource_version=resource_version)
            )
        try:
            self.coordination_v1_api.delete_namespaced_lease(
                name, namespace, _request_timeout=_request_timeout, **delete_kwargs
            )
        except client.ApiException as e:
            if e.status != 404 and not (e.status == 409 and resource_version is not None):
                raise

    def delete_sandbox_claims_by_label(
        self,
        namespace: str,
        label_selector: str,
        _request_timeout: float | tuple[float, float] | None = None,
    ) -> None:
        """Deletes every SandboxClaim matching a label selector with one deletecollection request.

        Args:
            _request_timeout: Optional timeout (seconds, or a ``(connect, read)``
                pair) forwarded to the underlying urllib3 request.
        """
        # Drain the response without decoding it because it lists every deleted claim and no caller
        # reads it. The sync client raises ApiException for a non-2xx response before returning it,
        # while the async client returns an unread response without checking its status, so only the
        # async helper checks the status.
        response = cast(
            urllib3.HTTPResponse,
            self._batch_custom_objects_api.delete_collection_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=CLAIM_PLURAL_NAME,
                label_selector=label_selector,
                _preload_content=False,
                _request_timeout=_request_timeout,
            ),
        )
        try:
            for _ in response.stream(decode_content=False):
                pass
        finally:
            # urllib3's release_conn() returns an unfinished response's socket to the pool in
            # 'Request-sent' state, so close the response first if draining stopped before EOF.
            if not response.closed:
                response.close()
            response.release_conn()

    def get_sandbox_warmpool(
        self, name: str, namespace: str, _request_timeout: float | tuple[float, float] | None = None
    ) -> dict[str, Any] | None:
        """Gets a SandboxWarmPool custom resource (or ``None`` if it doesn't exist)."""
        return self._get_extensions_object(WARMPOOL_PLURAL_NAME, name, namespace, _request_timeout)

    def get_sandbox_template(
        self, name: str, namespace: str, _request_timeout: float | tuple[float, float] | None = None
    ) -> dict[str, Any] | None:
        """Gets a SandboxTemplate custom resource (or ``None`` if it doesn't exist)."""
        return self._get_extensions_object(TEMPLATE_PLURAL_NAME, name, namespace, _request_timeout)

    def _get_extensions_object(
        self,
        plural: str,
        name: str,
        namespace: str,
        _request_timeout: float | tuple[float, float] | None,
    ) -> dict[str, Any] | None:
        """Gets an object in the extensions API group, or ``None`` if it doesn't exist."""
        try:
            return cast(dict[str, Any], self._batch_custom_objects_api.get_namespaced_custom_object(
                group=CLAIM_API_GROUP,
                version=CLAIM_API_VERSION,
                namespace=namespace,
                plural=plural,
                name=name,
                _request_timeout=_request_timeout,
            ))
        except client.ApiException as e:
            if e.status == 404:
                return None
            raise
