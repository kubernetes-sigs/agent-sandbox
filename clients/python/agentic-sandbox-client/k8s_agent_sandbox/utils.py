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

from kubernetes import client


def normalize_kubernetes_auth_config():
    """Normalize api_key to support both kubernetes <36.0.0 and >=36.0.0.

    Pre-36: Uses 'authorization' key
    36+: Uses 'BearerToken' key

    This ensures both keys exist with the same value, prioritizing 'BearerToken' if both present.
    """

    config = client.Configuration.get_default_copy()
    if config.api_key:
        bearer_token = config.api_key.get('BearerToken')
        authorization = config.api_key.get('authorization')

        if bearer_token and authorization:
            # Both exist - prioritize BearerToken
            config.api_key['authorization'] = bearer_token
        elif bearer_token:
            # Only BearerToken exists (k8s 36+) - copy to authorization
            config.api_key['authorization'] = bearer_token
        elif authorization:
            # Only authorization exists (k8s <36) - copy to BearerToken
            config.api_key['BearerToken'] = authorization

    client.Configuration.set_default(config)


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
