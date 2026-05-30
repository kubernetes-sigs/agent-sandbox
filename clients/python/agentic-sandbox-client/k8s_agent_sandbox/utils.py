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


def normalize_kubernetes_auth_config(client_module=None):
    """Ensure both 'authorization' and 'BearerToken' api_key entries are populated.

    Some versions of the kubernetes and kubernetes_asyncio clients expect
    'authorization'; others expect 'BearerToken'. This function copies the
    token from whichever key is present to the one that is missing, so both
    clients work regardless of which key was set by the config loader.

    Raises ValueError if both token keys or both prefix keys are set to
    different values, as proceeding with a mismatch will cause auth failures.
    api_key_prefix is mirrored using the same logic.

    Returns the (possibly modified) Configuration instance. Callers should
    pass it into ApiClient(configuration=...) rather than relying on the
    global default, to avoid cross-component side effects.

    Pass client_module to target a specific client's configuration
    (e.g. kubernetes_asyncio.client). Defaults to kubernetes.client.
    """
    if client_module is None:
        from kubernetes import client as client_module

    config = client_module.Configuration.get_default_copy()

    if config.api_key_prefix is not None:
        has_bearer_prefix = 'BearerToken' in config.api_key_prefix
        has_auth_prefix = 'authorization' in config.api_key_prefix
        if has_bearer_prefix and has_auth_prefix:
            if config.api_key_prefix['BearerToken'] != config.api_key_prefix['authorization']:
                raise ValueError(
                    "Both 'BearerToken' and 'authorization' api_key_prefix entries are set "
                    "with different values. Verify your kubeconfig — the Authorization header "
                    "prefix will differ depending on which key the installed client version reads."
                )

    if config.api_key is not None:
        has_bearer = 'BearerToken' in config.api_key
        has_auth = 'authorization' in config.api_key

        if has_bearer and has_auth:
            if config.api_key['BearerToken'] != config.api_key['authorization']:
                raise ValueError(
                    "Both 'BearerToken' and 'authorization' api_key entries are set with "
                    "different values. Verify your kubeconfig — authentication will fail "
                    "for whichever key the installed client version does not read."
                )
        elif has_bearer:
            config.api_key['authorization'] = config.api_key['BearerToken']
        elif has_auth:
            config.api_key['BearerToken'] = config.api_key['authorization']

    # Mirror prefix independently — even when both tokens were already present,
    # a missing prefix entry would cause the Authorization header to be malformed
    # for whichever client version reads the key that lacks a prefix.
    if config.api_key_prefix is not None:
        has_bearer_prefix = 'BearerToken' in config.api_key_prefix
        has_auth_prefix = 'authorization' in config.api_key_prefix
        if has_bearer_prefix and not has_auth_prefix:
            config.api_key_prefix['authorization'] = config.api_key_prefix['BearerToken']
        elif has_auth_prefix and not has_bearer_prefix:
            config.api_key_prefix['BearerToken'] = config.api_key_prefix['authorization']

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
