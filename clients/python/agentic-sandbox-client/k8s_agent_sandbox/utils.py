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
from functools import wraps
import threading

_patch_lock = threading.Lock()


def _safe_setattr(obj, attr, val):
    try:
        setattr(obj, attr, val)
    except AttributeError:
        pass


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


# TODO: This compatibility patch is a workaround for a permanent token auth format
# transition in the upstream kubernetes-client/python library (v36.0.0+), which uses
# the dictionary key 'BearerToken' instead of 'authorization' inside 'api_key'.
#
# We preserve backward compatibility with older packages (<36.0.0) where only
# 'authorization' is valid, and keep both keys synchronized under dynamic refreshes.
#
# Once we are sufficiently in the future (e.g., 6-12 months) and decide to bump
# the minimum supported version of 'kubernetes' to a post-v36 release that natively
# sets up 'BearerToken' properly, this compatibility logic can be safely dropped:
#   - Remove '_sync_k8s_bearer_token' and 'patch_k8s_config' helpers from 'utils.py'.
#   - Remove the patch calls from 'k8s_helper.py' and 'async_k8s_helper.py'.
#   - Delete the dedicated 'test_bearer_token_patch.py' unit and init test classes.
def _sync_k8s_bearer_token(config_obj):
    """Synchronizes 'authorization' and 'BearerToken' keys/prefixes in configuration dictionaries."""
    if config_obj is None:
        return

    def sync_dict(d, last_known_attr):
        if d is None:
            return
        auth_val = d.get("authorization")
        bearer_val = d.get("BearerToken")
        last_known = getattr(config_obj, last_known_attr, None)

        if auth_val == bearer_val:
            # Already in sync! Update state tracker.
            _safe_setattr(config_obj, last_known_attr, auth_val)
            return

        # Propagate the field that was actually modified
        if auth_val != last_known and bearer_val == last_known:
            d["BearerToken"] = auth_val
            _safe_setattr(config_obj, last_known_attr, auth_val)
        elif bearer_val != last_known and auth_val == last_known:
            d["authorization"] = bearer_val
            _safe_setattr(config_obj, last_known_attr, bearer_val)
        else:
            # Initial load or fallback synchronization
            if auth_val is not None and bearer_val is None:
                d["BearerToken"] = auth_val
                _safe_setattr(config_obj, last_known_attr, auth_val)
            elif bearer_val is not None and auth_val is None:
                d["authorization"] = bearer_val
                _safe_setattr(config_obj, last_known_attr, bearer_val)
            else:
                preferred = bearer_val if bearer_val is not None else auth_val
                d["authorization"] = preferred
                d["BearerToken"] = preferred
                _safe_setattr(config_obj, last_known_attr, preferred)

    if getattr(config_obj, "api_key", None) is not None:
        sync_dict(config_obj.api_key, "_last_known_bearer_token")

    if getattr(config_obj, "api_key_prefix", None) is not None:
        sync_dict(config_obj.api_key_prefix, "_last_known_bearer_token_prefix")


def patch_k8s_config(client_module):
    """Patches the active default configuration and refresh hook of a Kubernetes client module to keep token keys synchronized."""
    if not hasattr(client_module, "Configuration"):
        return
    with _patch_lock:
        try:
            c = client_module.Configuration.get_default_copy()
            if c is None:
                return
            if getattr(c, "_is_patched_for_bearer_token", False):
                return
            orig_hook = c.refresh_api_key_hook
            if orig_hook is not None and getattr(orig_hook, "_is_patched_for_bearer_token", False):
                return

            _sync_k8s_bearer_token(c)
            if orig_hook is not None:
                @wraps(orig_hook)
                def new_hook(cfg):
                    _sync_k8s_bearer_token(cfg)
                    try:
                        orig_hook(cfg)
                    finally:
                        _sync_k8s_bearer_token(cfg)
                new_hook._is_patched_for_bearer_token = True
                c.refresh_api_key_hook = new_hook

            _safe_setattr(c, "_is_patched_for_bearer_token", True)
            client_module.Configuration.set_default(c)
        except Exception:
            import logging
            logging.warning(
                "Failed to patch default Kubernetes configuration; bearer token compatibility workaround was not applied.",
                exc_info=True,
            )

