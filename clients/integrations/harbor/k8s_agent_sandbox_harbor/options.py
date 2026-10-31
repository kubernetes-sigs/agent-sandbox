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

"""Options a user passes on the Harbor command line with ``--ek key=value``.

Harbor hands every ``--ek`` pair to the environment constructor as a string
keyword argument. This model turns them into typed options, rejects unknown
keys early with the list of valid ones, and documents each knob in one place.
"""

from __future__ import annotations

import json
import os
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError, field_validator

ENV_PREFIX = "K8S_SANDBOX_HARBOR_"
# Harbor passes its own constructor arguments as keywords too; anything that is
# neither one of those nor one of our options is a misspelled --ek key.
_HARBOR_LEGACY_KWARGS = frozenset({"suppress_override_warnings"})


def _harbor_constructor_params() -> frozenset[str]:
    import inspect

    from harbor.environments.base import BaseEnvironment

    params = inspect.signature(BaseEnvironment.__init__).parameters
    return frozenset(name for name, p in params.items()
                     if p.kind not in (p.VAR_POSITIONAL, p.VAR_KEYWORD)) | _HARBOR_LEGACY_KWARGS


def _parse_mapping(value: Any) -> dict[str, str]:
    """``k=v,k2=v2`` or a JSON object, as a dict."""
    if value is None or value == "":
        return {}
    if isinstance(value, dict):
        return {str(k): str(v) for k, v in value.items()}
    text = str(value).strip()
    if text.startswith("{"):
        return {str(k): str(v) for k, v in json.loads(text).items()}
    out: dict[str, str] = {}
    for item in text.split(","):
        item = item.strip()
        if not item:
            continue
        key, sep, val = item.partition("=")
        if not sep:
            raise ValueError(f"expected key=value, got {item!r}")
        out[key.strip()] = val.strip()
    return out


class EnvironmentOptions(BaseModel):
    """Typed view of the ``--ek`` options of ``AgentSandboxEnvironment``."""

    model_config = ConfigDict(extra="forbid")

    namespace: str = "default"
    """Namespace for the templates, pools and claims."""
    kube_context: str | None = None
    """kubeconfig context; the current context when unset. Ignored in-cluster."""
    ready_timeout_s: int = Field(default=300, ge=1)
    """Seconds to wait for a claim to become Ready and its container to accept exec."""
    runtime_class: str | None = None
    """``runtimeClassName`` for the sandbox pod (for example ``gvisor``)."""
    node_selector: dict[str, str] = Field(default_factory=dict)
    """``k=v,k2=v2`` node selector for the sandbox pod."""
    tolerations: list[dict[str, Any]] = Field(default_factory=list)
    """JSON list of tolerations for the sandbox pod."""
    image_pull_secret: str | None = None
    """Name of an image pull Secret in ``namespace``."""
    image_mirror: str | None = None
    """Registry prefix that replaces the image's registry, e.g. an Artifact Registry remote."""
    image_map: str | None = None
    """Path to a JSON file ``{environment_id or task name: image}`` for tasks without ``docker_image``."""
    warm_replicas: int = Field(default=0, ge=0)
    """Replicas kept warm per task image. 0 creates every sandbox on demand."""
    cpu_limit_multiplier: float = Field(default=1.0, ge=0)
    """CPU limit as a multiple of the task's CPU request; 0 sets no CPU limit."""
    memory_limit_multiplier: float = Field(default=1.0, ge=0)
    """Memory limit as a multiple of the task's memory request; 0 sets no memory limit."""
    network: Literal["dns-public", "managed-default"] = "dns-public"
    """Template network policy: public internet plus cluster DNS, or the controller default."""
    claim_ttl_s: int = Field(default=6 * 3600, ge=0)
    """Shutdown time of each claim, a safety net when the harness dies; 0 sets none."""
    max_output_bytes: int = Field(default=0, ge=0)
    """Truncate exec output beyond this many bytes; 0 keeps everything."""
    exec_threads: int = Field(default=64, ge=1)
    """Threads for the blocking Kubernetes exec websockets, per process."""
    template_prefix: str = "hb"
    """Name prefix of the templates, pools and claims this package creates."""
    extra_labels: dict[str, str] = Field(default_factory=dict)
    """``k=v,...`` labels added to every object this package creates."""
    run_as_root: bool = True
    """Run the pod as uid 0, which Harbor's agent setup and ``su`` need."""

    @field_validator("node_selector", "extra_labels", mode="before")
    @classmethod
    def _mapping(cls, v: Any) -> dict[str, str]:
        return _parse_mapping(v)

    @field_validator("tolerations", mode="before")
    @classmethod
    def _tolerations(cls, v: Any) -> list[dict[str, Any]]:
        if v is None or v == "":
            return []
        if isinstance(v, str):
            v = json.loads(v)
        if not isinstance(v, list):
            raise ValueError("tolerations must be a JSON list")
        return v

    @classmethod
    def option_names(cls) -> list[str]:
        return list(cls.model_fields)

    @classmethod
    def from_kwargs(cls, kwargs: dict[str, Any]) -> "EnvironmentOptions":
        """Build options from ``--ek`` kwargs, with ``K8S_SANDBOX_HARBOR_*`` env defaults.

        Removes the consumed keys from ``kwargs`` so Harbor's base class sees
        only its own, and rejects a key that is neither ours nor Harbor's,
        because Harbor would otherwise ignore the typo silently.
        """
        values: dict[str, Any] = {}
        for name in cls.option_names():
            env_value = os.environ.get(ENV_PREFIX + name.upper())
            if env_value is not None:
                values[name] = env_value
            if name in kwargs:
                values[name] = kwargs.pop(name)
        names = ", ".join(cls.option_names())
        unknown = sorted(k for k in kwargs if k not in _harbor_constructor_params())
        if unknown:
            raise ValueError(f"unknown agent-sandbox environment option(s): {', '.join(unknown)}\n"
                             f"valid --ek keys: {names}")
        try:
            return cls(**values)
        except ValidationError as e:
            raise ValueError(f"invalid agent-sandbox environment options: {e}\n"
                             f"valid --ek keys: {names}") from e
