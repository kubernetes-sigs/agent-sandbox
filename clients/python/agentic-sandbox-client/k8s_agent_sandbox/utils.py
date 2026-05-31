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

from datetime import datetime, timedelta, timezone
from typing import Protocol


class _Configuration(Protocol):
    api_key: dict[str, str] | None
    api_key_prefix: dict[str, str] | None


class _ClientModule(Protocol):
    class Configuration:
        @staticmethod
        def get_default_copy() -> '_Configuration': ...


def normalize_kubernetes_auth_config(
    client_module: _ClientModule | None = None,
    configuration: _Configuration | None = None,
) -> _Configuration:
    """Ensure both 'authorization' and 'BearerToken' api_key entries are populated.

    Some versions of the kubernetes and kubernetes_asyncio clients expect
    'authorization'; others expect 'BearerToken'. This function copies the
    token from whichever key is present to the one that is missing, so both
    clients work regardless of which key was set by the config loader.

    Raises ValueError if both token keys are set to different values.
    When token keys are present, also raises ValueError if both api_key_prefix
    entries are set to different values (prefix mismatches are only validated
    when token-based auth is in use; unrelated prefix entries in cert/basic-auth
    configurations are left untouched). api_key_prefix is mirrored on the same
    condition.

    Returns the (possibly modified) Configuration instance. Callers should
    pass it into ApiClient(configuration=...) rather than relying on the
    global default, to avoid cross-component side effects.

    Pass an explicit configuration instance (loaded via client_configuration=
    on load_incluster_config / load_kube_config) to avoid touching the global
    default entirely. If not provided, falls back to
    client_module.Configuration.get_default_copy() (defaults to kubernetes.client).
    """
    if configuration is None:
        if client_module is None:
            try:
                from kubernetes import client as client_module
            except ImportError:
                raise ImportError(
                    "The 'kubernetes' package is not installed. Pass client_module= "
                    "(e.g. kubernetes_asyncio.client) or configuration= explicitly."
                ) from None
        configuration = client_module.Configuration.get_default_copy()

    config = configuration

    has_bearer = config.api_key is not None and 'BearerToken' in config.api_key
    has_auth = config.api_key is not None and 'authorization' in config.api_key

    # Only validate prefix mismatch when token-based auth is in use, so
    # cert/basic-auth configs with irrelevant prefix entries are not rejected.
    if (has_bearer or has_auth) and config.api_key_prefix is not None:
        has_bearer_prefix = 'BearerToken' in config.api_key_prefix
        has_auth_prefix = 'authorization' in config.api_key_prefix
        if has_bearer_prefix and has_auth_prefix:
            if config.api_key_prefix['BearerToken'] != config.api_key_prefix['authorization']:
                raise ValueError(
                    "Both 'BearerToken' and 'authorization' api_key_prefix entries are set "
                    "with different values. Verify your Kubernetes client configuration — "
                    "the Authorization header prefix will differ depending on which key "
                    "the installed client version reads."
                )

    if config.api_key is not None:
        if has_bearer and has_auth:
            if config.api_key['BearerToken'] != config.api_key['authorization']:
                raise ValueError(
                    "Both 'BearerToken' and 'authorization' api_key entries are set with "
                    "different values. Verify your Kubernetes client configuration — "
                    "authentication will fail for whichever key the installed client "
                    "version does not read."
                )
        elif has_bearer:
            config.api_key['authorization'] = config.api_key['BearerToken']
        elif has_auth:
            config.api_key['BearerToken'] = config.api_key['authorization']

    # Ensure api_key_prefix is a dict when token keys are actually present, then
    # mirror or initialize prefix entries so the Authorization header scheme is set.
    has_any_token = config.api_key is not None and bool(
        {'BearerToken', 'authorization'} & config.api_key.keys()
    )
    if has_any_token:
        if config.api_key_prefix is None:
            config.api_key_prefix = {}

        has_bearer_prefix = 'BearerToken' in config.api_key_prefix
        has_auth_prefix = 'authorization' in config.api_key_prefix

        if has_bearer_prefix and not has_auth_prefix:
            # Mirror the existing prefix to the missing key
            config.api_key_prefix['authorization'] = config.api_key_prefix['BearerToken']
        elif has_auth_prefix and not has_bearer_prefix:
            config.api_key_prefix['BearerToken'] = config.api_key_prefix['authorization']
        elif not has_bearer_prefix and not has_auth_prefix:
            # No prefix configured at all — set 'Bearer' as default for each token key present
            for key in ('BearerToken', 'authorization'):
                if key in config.api_key:
                    config.api_key_prefix[key] = 'Bearer'

    return config


def construct_sandbox_claim_lifecycle_spec(shutdown_after_seconds: int) -> dict[str, str]:
    """Construct a SandboxClaim lifecycle spec dict from a TTL in seconds.

    Returns a dict suitable for inclusion as ``spec.lifecycle`` in a
    SandboxClaim manifest, with ``shutdownTime`` set to *now + TTL* (UTC)
    and ``shutdownPolicy`` set to ``"Delete"``.

    Raises ``ValueError`` if the input is not a positive integer or is
    too large for datetime arithmetic.
    """
    if type(shutdown_after_seconds) is not int:
        raise ValueError(
            f"shutdown_after_seconds must be an integer, got {type(shutdown_after_seconds).__name__}"
        )
    if shutdown_after_seconds <= 0:
        raise ValueError(
            f"shutdown_after_seconds must be positive, got {shutdown_after_seconds}"
        )
    try:
        shutdown_time = datetime.now(timezone.utc) + timedelta(seconds=shutdown_after_seconds)
    except OverflowError:
        raise ValueError(
            f"shutdown_after_seconds is too large: {shutdown_after_seconds}"
        ) from None
    return {
        "shutdownTime": shutdown_time.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "shutdownPolicy": "Delete",
    }
