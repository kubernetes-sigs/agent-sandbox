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
    """Normalize api_key to support both kubernetes <36.0.0 and >=36.0.0.

    Pre-36: Uses 'authorization' key
    36+: Uses 'BearerToken' key

    Copies the token to whichever key is missing. If both keys are already set,
    no changes are made to avoid silently switching credentials.
    Pass client_module to use a different client (e.g. kubernetes_asyncio.client for async usage).
    """
    if client_module is None:
        from kubernetes import client as client_module

    config = client_module.Configuration.get_default_copy()
    if config.api_key:
        bearer_token = config.api_key.get('BearerToken')
        authorization = config.api_key.get('authorization')

        if bearer_token and not authorization:
            config.api_key['authorization'] = bearer_token
        elif authorization and not bearer_token:
            config.api_key['BearerToken'] = authorization

    client_module.Configuration.set_default(config)


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
