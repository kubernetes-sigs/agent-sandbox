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

"""Credentials for the hub cluster, shared by the reader and the writer.

Both halves of the ClusterProfile integration talk to a cluster they do NOT
run in -- the planner reads every ClusterProfile, each member applies its own
status -- so in-cluster config is wrong by construction and both need explicit
hub credentials. This module is the one place that decides how to get them.

WHY THIS EXISTS AT ALL
----------------------
The obvious answer on GKE is a kubeconfig with the standard exec stanza:

    users:
    - name: hub
      user:
        exec:
          command: gke-gcloud-auth-plugin

That requires the plugin binary in the image. The fleet-member image is
`python:3.12-slim` with the SDK and nothing else, and adding the Google Cloud
SDK to it -- on a container that runs one replica per cluster and whose whole
job is a capacity loop -- is a large dependency for one HTTP request.

So `gke-metadata` does that HTTP request directly. Under Workload Identity the
GKE metadata server hands back a token for the GSA bound to the pod's
KubernetesServiceAccount, which is exactly what the plugin would have returned.

The payoff is not just image size. A kubeconfig that carries no credentials
holds nothing secret -- an API server address and a CA certificate are both
public -- so it ships as a **ConfigMap** rather than a Secret, with no key
material on disk, no rotation, and nothing to leak if it is committed. See
`deploy/gen-hub-kubeconfig.sh`, which generates exactly that.

TOKEN REFRESH
-------------
Access tokens last an hour and these processes run for weeks, so a token
fetched at startup is not enough. `Configuration.refresh_api_key_hook` is the
client's sanctioned seam for this -- `get_api_key_with_prefix()` calls it on
every request, and it is the same mechanism the client's own OIDC and GCP
providers use. We refresh a few minutes early rather than on expiry, because
expiring mid-flight surfaces as a 401 that looks like an RBAC problem and will
cost somebody an afternoon.
"""

from __future__ import annotations

import json
import logging
import os
import time
import urllib.request
from typing import Any, Callable

logger = logging.getLogger("agent_sandbox_fleet.hubauth")

TOKEN_SOURCE_KUBECONFIG = "kubeconfig"
TOKEN_SOURCE_GKE_METADATA = "gke-metadata"
TOKEN_SOURCES = (TOKEN_SOURCE_KUBECONFIG, TOKEN_SOURCE_GKE_METADATA)

GKE_METADATA_TOKEN_URL = (
    "http://metadata.google.internal/computeMetadata/v1/"
    "instance/service-accounts/default/token"
)

# Refresh this many seconds before the token actually expires.
_EXPIRY_SKEW_S = 300
# Assumed lifetime when the metadata server omits expires_in. Deliberately
# short: guessing low costs an extra fetch, guessing high costs a 401 storm.
_DEFAULT_LIFETIME_S = 600


class GkeMetadataTokenProvider:
    """Caches a Workload Identity access token from the GKE metadata server.

    Equivalent to what `gke-gcloud-auth-plugin` returns, without the binary.
    """

    def __init__(
        self,
        url: str = GKE_METADATA_TOKEN_URL,
        *,
        fetcher: Callable[[str], dict[str, Any]] | None = None,
        clock: Callable[[], float] = time.monotonic,
    ):
        self._url = url
        self._fetch = fetcher or _fetch_metadata_token
        self._clock = clock
        self._token: str | None = None
        self._expires_at: float = 0.0

    def token(self) -> str:
        """The current access token, fetching or refreshing as needed."""
        now = self._clock()
        if self._token is not None and now < self._expires_at:
            return self._token

        payload = self._fetch(self._url)
        token = payload.get("access_token")
        if not token:
            raise RuntimeError(
                f"metadata server at {self._url} returned no access_token. On GKE "
                f"this usually means Workload Identity is not enabled on the node "
                f"pool, or the ServiceAccount lacks the "
                f"iam.gke.io/gcp-service-account annotation."
            )
        lifetime = int(payload.get("expires_in") or _DEFAULT_LIFETIME_S)
        self._token = token
        self._expires_at = now + max(lifetime - _EXPIRY_SKEW_S, 1)
        logger.debug("refreshed hub token, valid for %ss", lifetime)
        return token


def _fetch_metadata_token(url: str) -> dict[str, Any]:
    req = urllib.request.Request(url, headers={"Metadata-Flavor": "Google"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        raise RuntimeError(
            f"could not reach the GKE metadata server at {url}: {e}. This path "
            f"only works from inside a GKE pod with Workload Identity; use "
            f"--hub-token-source=kubeconfig when running off-cluster."
        ) from e


def load_hub_configuration(
    *,
    kubeconfig: str | None = None,
    context: str | None = None,
    token_source: str = TOKEN_SOURCE_KUBECONFIG,
    token_provider: Any = None,
) -> Any:
    """Build a `kubernetes.client.Configuration` pointed at the hub.

    Args:
      kubeconfig: Path to the hub kubeconfig. Under `gke-metadata` this supplies
        only the server address and CA -- any credentials in it are ignored.
      context: Context to select within that kubeconfig.
      token_source: `kubeconfig` to use whatever the file authenticates with, or
        `gke-metadata` to authenticate as the pod's Workload Identity GSA.
      token_provider: Injectable provider for tests; anything with `.token()`.
    """
    if token_source not in TOKEN_SOURCES:
        raise ValueError(
            f"unknown token source {token_source!r}; expected one of "
            f"{', '.join(TOKEN_SOURCES)}"
        )

    try:
        from kubernetes import client as k8s_client, config as k8s_config
    except ImportError as e:  # pragma: no cover - dependency guard
        raise RuntimeError(
            "talking to a hub needs the kubernetes client; `pip install kubernetes`"
        ) from e

    # Resolve the file before handing it to the client. With config_file=None
    # load_kube_config falls back to $KUBECONFIG or ~/.kube/config, and in the
    # fleet-member pod neither exists -- the failure surfaces as a bare
    # ConfigException naming ~/.kube/config, which reads like a mounting
    # mistake rather than a missing flag. Worse off-cluster: the default
    # kubeconfig is usually the operator's MEMBER-cluster context, so the
    # publisher would happily write ClusterProfiles into the wrong apiserver.
    #
    # This applies to gke-metadata too. That mode replaces the credentials, not
    # the file: the address and CA bundle still come from here.
    path = kubeconfig or os.environ.get("KUBECONFIG") or os.path.expanduser(
        "~/.kube/config")
    if not os.path.exists(path):
        raise RuntimeError(
            f"hub kubeconfig {path!r} does not exist. Pass --hub-kubeconfig (or "
            f"set KUBECONFIG). Note that --hub-token-source=gke-metadata still "
            f"needs this file -- it supplies the hub's address and CA bundle, "
            f"and only the credentials in it are ignored."
        )

    cfg = k8s_client.Configuration()
    # Loaded in both modes: even when the credentials are ignored, this is what
    # resolves the context, the server URL and the CA bundle (including writing
    # certificate-authority-data out to a temp file, which is fiddly to redo).
    try:
        k8s_config.load_kube_config(
            config_file=path, context=context, client_configuration=cfg,
        )
    except Exception as e:
        raise RuntimeError(
            f"could not load hub kubeconfig {path!r}"
            + (f" (context {context!r})" if context else "")
            + f": {e}"
        ) from e

    if token_source == TOKEN_SOURCE_KUBECONFIG:
        return cfg

    provider = token_provider or GkeMetadataTokenProvider()

    # Override whatever the kubeconfig set up. The hook fires on every request,
    # so this both installs the token and keeps it fresh; assigning api_key once
    # would work for an hour and then start 401ing.
    def _refresh(configuration: Any) -> None:
        # Both keys, for the same reason the prefix is set under both: the lookup
        # is by security-scheme name with the alias only as a fallback. Writing
        # both from inside the hook -- rather than seeding 'BearerToken' once
        # outside it -- is what keeps this safe. A static copy would win the
        # lookup and pin the very first token forever, reintroducing the hourly
        # 401 the hook exists to prevent.
        token = provider.token()
        configuration.api_key["authorization"] = token
        configuration.api_key["BearerToken"] = token

    # THE PREFIX GOES UNDER TWO KEYS, AND BOTH ARE LOAD-BEARING.
    #
    # `configuration.api_key_prefix['authorization'] = 'Bearer'` is the idiom in
    # every example you will find, and on kubernetes>=36 it is not enough. The
    # generated client resolves credentials by SECURITY SCHEME name:
    #
    #     get_api_key_with_prefix('BearerToken', alias='authorization')
    #       key    = api_key.get('BearerToken', api_key.get('authorization'))
    #       prefix = api_key_prefix.get('BearerToken')     # <- no alias fallback
    #
    # The key still resolves through the alias -- that fallback is real, see
    # configuration.py -- but the prefix, looked up under the scheme name only,
    # silently does not. The header then goes out as a bare `ya29....` with no
    # `Bearer `, and GKE answers 401 with the same body it uses for an anonymous
    # request: indistinguishable, from the client, from a Workload Identity or
    # RBAC problem.
    #
    # Setting both keys works on old and new clients alike, and costs nothing if
    # a future release drops the alias fallback on the key as well.
    cfg.api_key_prefix["authorization"] = "Bearer"
    cfg.api_key_prefix["BearerToken"] = "Bearer"
    cfg.refresh_api_key_hook = _refresh
    _refresh(cfg)
    _assert_bearer_header(cfg)
    return cfg


def _assert_bearer_header(cfg: Any) -> None:
    """Fail at startup if the client would send an unusable Authorization header.

    This guards a failure that is otherwise invisible until it reaches the hub
    and comes back as a generic 401. It asks the configuration the same question
    the request path asks it, and checks the answer rather than checking the
    inputs we fed in -- the distinction matters, because the inputs were correct
    in the bug this was written for.
    """
    auth_settings = getattr(cfg, "auth_settings", None)
    if not callable(auth_settings):  # a stub in tests, nothing to verify
        return
    entry = (auth_settings() or {}).get("BearerToken")
    if entry is None:
        raise RuntimeError(
            "the kubernetes client will not send a bearer token to the hub: "
            "auth_settings() has no 'BearerToken' entry even though api_key was "
            "populated. The client's credential plumbing has changed shape; "
            "compare Configuration.auth_settings() against hubauth.py."
        )
    value = entry.get("value") or ""
    if not value.startswith("Bearer "):
        # Length only. The value IS the credential, and this RuntimeError ends up
        # in pod logs and bug reports; even a short slice of a live OAuth token
        # does not belong there. The length is enough to tell "empty" from
        # "present but unprefixed", which is the only distinction the operator
        # needs to act on.
        raise RuntimeError(
            f"the hub Authorization header would be sent without its 'Bearer ' "
            f"prefix ({len(value)} chars; value redacted). GKE rejects this as "
            f"401 Unauthorized, which looks exactly like a Workload Identity "
            f"failure and is not one. api_key_prefix is probably being looked up "
            f"under a key hubauth.py does not set."
        )
