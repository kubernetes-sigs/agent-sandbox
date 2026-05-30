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

    Raises ValueError if both keys are set to different values, as proceeding
    with mismatched credentials will cause auth failures. api_key_prefix is
    mirrored using the same logic.

    Returns the (possibly modified) Configuration instance. Callers should
    pass it into ApiClient(configuration=...) rather than relying on the
    global default, to avoid cross-component side effects.

    Pass client_module to target a specific client's configuration
    (e.g. kubernetes_asyncio.client). Defaults to kubernetes.client.
    """
    if client_module is None:
        from kubernetes import client as client_module

    config = client_module.Configuration.get_default_copy()

    if config.api_key:
        bearer_token = config.api_key.get('BearerToken')
        authorization = config.api_key.get('authorization')

        if bearer_token and authorization and bearer_token != authorization:
            raise ValueError(
                "Both 'BearerToken' and 'authorization' api_key entries are set with "
                "different values. Verify your kubeconfig — authentication will fail "
                "for whichever key the installed client version does not read."
            )
        elif bearer_token and not authorization:
            config.api_key['authorization'] = bearer_token
            if config.api_key_prefix and 'authorization' not in config.api_key_prefix:
                bearer_prefix = config.api_key_prefix.get('BearerToken')
                if bearer_prefix:
                    config.api_key_prefix['authorization'] = bearer_prefix
        elif authorization and not bearer_token:
            config.api_key['BearerToken'] = authorization
            if config.api_key_prefix and 'BearerToken' not in config.api_key_prefix:
                auth_prefix = config.api_key_prefix.get('authorization')
                if auth_prefix:
                    config.api_key_prefix['BearerToken'] = auth_prefix

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
