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

"""DeerFlow SandboxProvider backed by agent-sandbox.

Drop-in replacement for DeerFlow's OpenSandbox backend: point DeerFlow at it
with

    sandbox:
      use: agent_sandbox_provider.provider:AgentSandboxProvider

Each DeerFlow sandbox becomes a Kubernetes pod claimed through a
SandboxWarmPool (env AGENT_SANDBOX_WARMPOOL, default "deer-flow-warmpool") in
namespace env AGENT_SANDBOX_NAMESPACE (default "default"). Commands run over
sandboxd's gRPC ProcessService and file operations over its REST filesystem,
both reached through a pod port-forward opened by the k8s-agent-sandbox SDK.
Cluster credentials come from the ambient kubeconfig (KUBECONFIG).
"""

from __future__ import annotations

import errno
import hashlib
import io
import logging
import os
import posixpath
import re
import shlex
import threading
import uuid

from deerflow.config import get_app_config
from deerflow.sandbox.download import DEFAULT_DOWNLOAD_LIMIT, download_limit
from deerflow.sandbox.identity import derive_sandbox_scope_token
from deerflow.sandbox.remote_search import parse_remote_search_output, remote_search_command
from deerflow.sandbox.sandbox import Sandbox, _validate_extra_env
from deerflow.sandbox.sandbox_provider import SandboxProvider
from deerflow.sandbox.search import GrepMatch, path_matches, should_ignore_path_under_root, truncate_line
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxdPodTunnelConnectionConfig

from . import parsing

logger = logging.getLogger(__name__)

# sandboxd runs with --root-dir=/mnt/user-data in the example template, and
# DeerFlow confines artifact downloads to /mnt/user-data as well.
ROOT_DIR = "/mnt/user-data"

DEFAULT_NAMESPACE = "default"
DEFAULT_WARMPOOL = "deer-flow-warmpool"
DEFAULT_READY_TIMEOUT = 300
# Wall-clock ceiling for a command when the caller passes no timeout, matching
# the reference providers (OpenSandbox DEFAULT_COMMAND_TIMEOUT and
# LocalSandbox DEFAULT_COMMAND_TIMEOUT_SECONDS are both 600).
DEFAULT_COMMAND_TIMEOUT = 600.0


def _append_output(output: str, value: str) -> str:
    """Append a non-empty stream to the accumulated output, newline-safe.

    Same helper shape the reference OpenSandbox adapter uses to join stdout,
    stderr and error details into the command's text result.
    """
    if not value:
        return output
    if not output or output.endswith("\n"):
        return output + value
    return f"{output}\n{value}"


class AgentSandbox(Sandbox):
    """DeerFlow tool surface implemented on the k8s-agent-sandbox SDK."""

    #: Every execute_command is one fresh sandboxd ProcessService call; no
    #: shell state survives between calls. Declared explicitly (as every
    #: shipped provider does) so evidence consumers can trust fresh-shell
    #: semantics instead of degrading recorded bash to UNVERIFIED.
    persistent_shell_sessions = False

    def __init__(self, id: str, sdk_sandbox, root: str = ROOT_DIR, default_command_timeout: float = DEFAULT_COMMAND_TIMEOUT):
        super().__init__(id)
        # sdk_sandbox is k8s_agent_sandbox.sandbox.Sandbox; typed loosely so
        # this module imports without the grpc extra installed.
        self._sbx = sdk_sandbox
        self._root = root
        self._default_command_timeout = default_command_timeout

    # -- helpers ----------------------------------------------------------

    def _rel(self, path: str) -> str:
        return parsing.to_root_relative(path, self._root)

    def _confined(self, path: str) -> str:
        """Canonicalize a caller path and confine it under the sandbox root.

        Returns the normalized absolute path (traversal segments resolved,
        PermissionError if the result escapes the root) so shell commands and
        root-prefix matching never operate on the raw caller input.
        """
        rel = self._rel(path)
        return self._root if rel == "." else posixpath.join(self._root, rel)

    def _resolve_command_timeout(self, timeout: float | None) -> float | None:
        """Map the contract's ``timeout`` onto sandboxd's command deadline.

        ``None`` takes the provider default so the SDK reports exceeded
        deadlines as a ``timed_out`` result with exit code 124 (without a
        ``command_timeout`` the SDK disables that reporting and a slow command
        dies as a raw gRPC error instead). ``0`` is this example's "no limit"
        sentinel (same convention as ``idle_timeout: 0`` in the README): pass
        no ``command_timeout`` and no outer wait, so sandboxd dials the gRPC
        call without a deadline.
        """
        if timeout is None:
            return self._default_command_timeout
        value = float(timeout)
        if value == 0:
            return None
        if value < 0:
            raise ValueError(f"timeout must be non-negative, got {timeout}")
        return value

    def _run(self, command: str, timeout: float | None = None):
        # The sandboxd runtime uses ``command_timeout`` as the gRPC deadline
        # (ignoring the outer ``timeout``) when set; passing ``timeout=None``
        # with ``command_timeout=None`` means "no deadline". This example is
        # sandboxd-only (SandboxdPodTunnelConnectionConfig); the legacy REST
        # runtime would need a finite outer wait here.
        return self._sbx.commands.run(
            command,
            timeout=None,
            command_timeout=self._resolve_command_timeout(timeout),
        )

    # -- DeerFlow Sandbox surface ------------------------------------------

    def execute_command(self, command, env=None, timeout=None) -> str:
        # Contract-mandated POSIX env-name check (defense-in-depth for
        # shell-using implementations; see deerflow.sandbox.sandbox).
        _validate_extra_env(env)
        try:
            result = self._sbx.commands.run(
                command,
                timeout=None,
                command_timeout=self._resolve_command_timeout(timeout),
                env=env,
            )
        except Exception as exc:
            # Transport/launch failures are rendered as tool output text,
            # like the reference remote provider, so the agent (and its
            # exit-code marker parser) sees the failure instead of the turn
            # dying on an exception.
            logger.error("Failed to execute command in agent-sandbox %s: %s", self.id, exc)
            return f"Error: {exc}"
        # Mirror the reference remote provider's text contract: consumers
        # parse the trailing "\nExit Code: N" marker (tools.py's
        # _BASH_EXIT_MARKER_TAIL_RE), and a timeout must surface as exit code
        # 124, not an exception — sandboxd reports deadline-exceeded results
        # with exit_code=124 already; note that partial output is not
        # recoverable through the gRPC deadline.
        output = _append_output(result.stdout or "", result.stderr or "")
        if result.exit_code != 0:
            output = (
                f"{output}\nExit Code: {result.exit_code}"
                if output
                else f"Command exited with code {result.exit_code}"
            )
        return output if output else "(no output)"

    def read_file(self, path, start_line=None, end_line=None) -> str:
        try:
            content = self._sbx.files.read(self._rel(path)).decode("utf-8", errors="replace")
        except PermissionError:
            raise
        except Exception as e:
            raise OSError(f"cannot read {path} from sandbox {self.id}: {e}") from e
        if start_line is None and end_line is None:
            # Unbounded reads return the fetched content untouched, like the
            # reference remote providers; slicing only applies to windows.
            return content
        return parsing.slice_lines(content, start_line, end_line)

    def download_file(self, path, *, max_bytes=None) -> bytes:
        # Caller may tighten but never raise the provider cap; download_limit
        # validates the type (non-negative int or None) exactly as the
        # harness contract requires.
        limit = download_limit(max_bytes, DEFAULT_DOWNLOAD_LIMIT)
        buffer = io.BytesIO()
        try:
            # Stream with the cap enforced DURING reading (the SDK's read_to
            # stops at the first overflowing chunk); an unbounded files.read
            # would buffer a 2GB artifact into the gateway before EFBIG could
            # be raised.
            self._sbx.files.read_to(self._rel(path), buffer, max_bytes=limit)
        except PermissionError:
            raise
        except RuntimeError as e:
            if "exceeds limit" in str(e):
                # Never return a truncated prefix as success (DeerFlow
                # contract); the buffer's partial content is discarded.
                raise OSError(errno.EFBIG, f"File exceeds maximum download size of {limit} bytes", path) from e
            raise OSError(f"cannot download {path} from sandbox {self.id}: {e}") from e
        except Exception as e:
            raise OSError(f"cannot download {path} from sandbox {self.id}: {e}") from e
        return buffer.getvalue()

    def list_dir(self, path, max_depth=2) -> list[str]:
        # Canonicalize first so traversal segments in the caller's path never
        # reach the queue (child names are built from this string).
        path = self._confined(path)
        rel = self._rel(path)
        if not self._sbx.files.exists(rel):
            raise FileNotFoundError(f"no such directory in sandbox {self.id}: {path}")
        results: list[str] = []
        queue: list[tuple[str, int]] = [(path.rstrip("/") or "/", 0)]
        while queue:
            current, depth = queue.pop(0)
            try:
                entries = self._sbx.files.list(self._rel(current))
            except Exception as e:
                raise OSError(f"cannot list {current} in sandbox {self.id}: {e}") from e
            for entry in entries:
                child = f"{current}/{entry.name}"
                if entry.type == "directory":
                    results.append(child + "/")
                    if depth + 1 < max_depth:
                        queue.append((child, depth + 1))
                else:
                    results.append(child)
        return results

    def write_file(self, path, content, append=False) -> None:
        rel = self._rel(path)
        data = content.encode("utf-8")
        if append and self._sbx.files.exists(rel):
            # Single deterministic path: read existing bytes, append, rewrite.
            data = self._sbx.files.read(rel) + data
        self._sbx.files.write(rel, data)

    def glob(self, path, pattern, *, include_dirs=False, max_results=200):
        # Semantics follow the reference remote search (search.py
        # path_matches/should_ignore_name): DeerFlow glob patterns match the
        # FULL path relative to the search root — `find -name` matches
        # basenames only, so the idiomatic '**/*.py' would have returned an
        # authoritative "no matches". The search is bounded and status-
        # marked through remote_search_command so a missing root or a failed
        # find is never read as "no matches" (#5376).
        if max_results <= 0:
            raise ValueError("max_results must be positive")
        # Search the canonicalized, root-confined path: the raw input could
        # carry traversal segments past sandboxd's root dir, and the root
        # prefix logic below must match what find actually prints.
        path = self._confined(path)
        types = ("f", "d") if include_dirs else ("f",)
        type_expr = " -o ".join(f"-type {entry_type}" for entry_type in types)
        hard_limit = max(max_results * 4, max_results + 50)
        quoted = shlex.quote(path)
        search = f"find -H {quoted} \\( {type_expr} \\) -print 2>/dev/null"
        result = self._run(remote_search_command(search, path, limit=hard_limit))
        if result.exit_code != 0:
            raise OSError(f"glob failed in sandbox {self.id}: {result.stderr or result.stdout}")
        output = parse_remote_search_output(result.stdout, path, tool="find", limit=hard_limit)

        matches: list[str] = []
        root = path.rstrip("/") or "/"
        root_prefix = root if root == "/" else f"{root}/"
        # Records are LF-delimited; other splitlines() boundaries can be
        # characters in a path. Do NOT strip: whitespace can be part of a
        # filename.
        for entry in output.text.split("\n"):
            if not entry or (entry != root and not entry.startswith(root_prefix)) or should_ignore_path_under_root(entry, root):
                continue
            relative = entry[len(root):].lstrip("/")
            if relative and path_matches(pattern, relative):
                matches.append(entry)
                # Look one match past the cap before deciding, like the
                # reference: an exhausted tree of exactly max_results matches
                # is complete, not truncated.
                if len(matches) > max_results:
                    return matches[:max_results], True
        return matches, output.truncated

    def grep(self, path, pattern, *, glob=None, literal=False,
             case_sensitive=False, max_results=100):
        if max_results <= 0:
            raise ValueError("max_results must be positive")
        if not literal:
            # Validate the regex up front (it is the client-side matcher for
            # results filtered past grep -E's superset), raising re.error on
            # a bad pattern like the reference.
            re.compile(pattern, 0 if case_sensitive else re.IGNORECASE)
        # As in glob(): grep -r and the root-prefix filtering below operate
        # on the canonicalized, root-confined path, never the raw input.
        path = self._confined(path)
        flags = ["-r", "-H", "-n", "-I"]
        if not case_sensitive:
            flags.append("-i")
        flags.append("-F" if literal else "-E")
        portable_flags = list(flags)
        if glob is not None:
            # Cheap server-side prefilter on the basename; the authoritative
            # full-path decision is the client-side path_matches below.
            include_pattern = glob.split("/")[-1] or glob
            flags.append(shlex.quote(f"--include={include_pattern}"))
        per_file_cap = max(max_results + 1, 50)
        flags.append(f"-m{per_file_cap}")
        hard_limit = max(max_results * 4, max_results + 50)
        arguments = f" -e {shlex.quote(pattern)} {shlex.quote(path)} 2>/dev/null"
        primary = "grep " + " ".join(flags) + arguments
        fallback = "grep " + " ".join(portable_flags) + arguments
        # Retry without --include/-m only when grep errors (status 2; BusyBox
        # lacks them); a missing grep (127) must not read as "no matches".
        search = f'{primary}; status=$?; if [ "$status" -eq 2 ]; then {fallback}; status=$?; fi; (exit "$status")'
        result = self._run(remote_search_command(search, path, limit=hard_limit))
        if result.exit_code != 0:
            raise OSError(f"grep failed in sandbox {self.id}: {result.stderr or result.stdout}")
        output = parse_remote_search_output(result.stdout, path, tool="grep", limit=hard_limit)

        root = path.rstrip("/") or "/"
        root_prefix = root if root == "/" else f"{root}/"
        hits: list[GrepMatch] = []
        seen_positions: set[tuple[str, int]] = set()
        for raw in output.text.split("\n"):
            parsed = parsing.parse_grep_line(raw)
            if parsed is None:
                continue
            hit_path, line_number, line = parsed
            if should_ignore_path_under_root(hit_path, root):
                continue
            if glob is not None:
                if hit_path != root and not hit_path.startswith(root_prefix):
                    continue
                relative = posixpath.basename(hit_path) if hit_path == root else hit_path[len(root):].lstrip("/")
                if not path_matches(glob, relative):
                    continue
            position = (hit_path, line_number)
            if position in seen_positions:
                continue
            seen_positions.add(position)
            hits.append(GrepMatch(path=hit_path, line_number=line_number, line=truncate_line(line)))
            if len(hits) > max_results:
                return hits[:max_results], True
        return hits, output.truncated

    def update_file(self, path, content: bytes) -> None:
        self._sbx.files.write(self._rel(path), content)


class AgentSandboxProvider(SandboxProvider):
    """Creates one SandboxClaim per DeerFlow sandbox scope, via the SDK."""

    def __init__(self):
        sandbox_cfg = get_app_config().sandbox
        self._namespace = os.getenv("AGENT_SANDBOX_NAMESPACE", DEFAULT_NAMESPACE)
        self._warmpool = os.getenv("AGENT_SANDBOX_WARMPOOL", DEFAULT_WARMPOOL)
        ready_timeout = getattr(sandbox_cfg, "ready_timeout", None) or DEFAULT_READY_TIMEOUT
        self._ready_timeout = int(ready_timeout)
        # Same knob the reference OpenSandbox provider reads: the default
        # per-command ceiling when callers pass no timeout. 0/None is not a
        # valid default (mirrors the reference's "must be positive").
        raw_command_timeout = getattr(sandbox_cfg, "bash_command_timeout", None)
        try:
            command_timeout = float(DEFAULT_COMMAND_TIMEOUT if raw_command_timeout is None else raw_command_timeout)
        except (TypeError, ValueError) as exc:
            raise ValueError("sandbox.bash_command_timeout must be positive") from exc
        if command_timeout <= 0:
            raise ValueError("sandbox.bash_command_timeout must be positive")
        self._default_command_timeout = command_timeout
        # DeerFlow's sandbox_timeout (total lifetime) maps onto the claim TTL;
        # idle_timeout is not enforced — release() destroys the claim.
        sandbox_timeout = getattr(sandbox_cfg, "sandbox_timeout", 0) or 0
        self._shutdown_after_seconds = int(sandbox_timeout) if sandbox_timeout > 0 else None
        self._client = SandboxClient(
            connection_config=SandboxdPodTunnelConnectionConfig(),
        )
        # sandbox_id -> (claim_name, AgentSandbox)
        self._sandboxes: dict[str, tuple[str, AgentSandbox]] = {}
        # DeerFlow tool turns can drive acquire/release from several threads;
        # the lock serializes the cache check-create-store sequence so
        # concurrent acquires of one scope share a single SDK connection
        # instead of leaking all but the last.
        self._lock = threading.RLock()

    @staticmethod
    def _claim_name(user_id: str | None, thread_id: str) -> str:
        # Scope by (user_id, thread_id) through the harness's durable scope
        # token, like the reference OpenSandbox provider: a thread id shared
        # across gateway users (e.g. a channel thread) must not name-collide
        # into the same claim and adopt another user's live pod.
        token = derive_sandbox_scope_token(user_id=user_id or "", thread_id=thread_id)
        return f"deerflow-{token}"

    def acquire(self, thread_id=None, *, user_id=None) -> str:
        if thread_id is None:
            # Anonymous scope: like the reference provider, mint a fresh
            # sandbox per acquire; a deterministic "default" name would hand
            # every thread-less caller the same pod.
            claim_name = f"deerflow-{uuid.uuid4().hex[:12]}"
            adopt_existing = False
        else:
            claim_name = self._claim_name(user_id, thread_id)
            adopt_existing = True
        sandbox_id = f"{self._namespace}/{claim_name}"
        # Held across the whole check-create-store sequence (create_sandbox
        # included): a concurrent acquire of the same scope blocks and then
        # finds the cached connection, instead of both missing the cache and
        # the second store overwriting (leaking) the first AgentSandbox. The
        # trade-off is serialized cold starts across scopes, which is fine
        # for this example's concurrency.
        with self._lock:
            cached = self._sandboxes.get(sandbox_id)
            if cached is not None:
                if self._is_live(cached[1]):
                    return sandbox_id
                # The controller's claim TTL (shutdownTime) deletes claim and
                # pod underneath us; a dead cache entry would block
                # re-creation for this scope forever.
                logger.info("dropping cached agent-sandbox %s: backend no longer present", sandbox_id)
                self._sandboxes.pop(sandbox_id, None)
                try:
                    cached[1]._sbx.close_connection()
                except Exception:
                    logger.debug("failed to close dead connection for %s", sandbox_id, exc_info=True)
            labels = {"deerflow.example/thread": hashlib.sha1(thread_id.encode()).hexdigest()[:12]} if thread_id else {}
            if user_id:
                labels["deerflow.example/user"] = hashlib.sha1(user_id.encode()).hexdigest()[:12]
            # With adopt_existing the SDK attaches to a live claim on 409. If
            # a later step then fails (e.g. readiness timeout), rolling back
            # would delete a pod the user is already running on — including
            # its /mnt/user-data state. Record beforehand whether the claim
            # pre-existed so the rollback below only deletes claims this
            # call would have created.
            claim_existed = adopt_existing and (
                self._client.k8s_helper.get_sandbox_claim(claim_name, self._namespace) is not None
            )
            try:
                sdk_sandbox = self._client.create_sandbox(
                    warmpool=self._warmpool,
                    namespace=self._namespace,
                    sandbox_ready_timeout=self._ready_timeout,
                    claim_name=claim_name,
                    adopt_existing=adopt_existing,
                    shutdown_after_seconds=self._shutdown_after_seconds,
                    labels=labels,
                )
            except Exception as e:
                # The SDK only rolls back generated claim names; ours is
                # explicit, so a readiness timeout would otherwise leak the
                # claim (and pod, when no TTL is configured). Best-effort
                # delete: a 404 (claim was never created) is success, and the
                # acquire error is what the caller sees regardless. Claims
                # that pre-existed (adopted, not created here) are left
                # alone.
                if not claim_existed:
                    try:
                        self._client.k8s_helper.delete_sandbox_claim(claim_name, self._namespace)
                    except Exception:
                        logger.warning("failed to roll back claim %s after acquire error", claim_name, exc_info=True)
                raise RuntimeError(
                    f"failed to acquire agent-sandbox claim {claim_name} "
                    f"(warmpool {self._warmpool}, namespace {self._namespace}): {e}"
                ) from e
            self._sandboxes[sandbox_id] = (claim_name, AgentSandbox(sandbox_id, sdk_sandbox, default_command_timeout=self._default_command_timeout))
        logger.info("acquired agent-sandbox %s for thread %s", sandbox_id, thread_id)
        return sandbox_id

    @staticmethod
    def _is_live(sandbox: "AgentSandbox") -> bool:
        """Whether a cached sandbox still exists server-side.

        ``Sandbox.is_active`` is client-side only (connection open, engines
        initialized), so it cannot see a TTL-deleted claim; the status check
        reads the bound Sandbox object through the API.
        """
        sdk_sandbox = sandbox._sbx
        if not sdk_sandbox.is_active:
            return False
        try:
            status, _ = sdk_sandbox.status()
        except Exception:
            # API flakiness is not evidence of death; keep the entry and let
            # the next real operation surface transport errors.
            return True
        return status != "SandboxNotFound"

    def get(self, sandbox_id):
        entry = self._sandboxes.get(sandbox_id)
        return entry[1] if entry else None

    def sync_agent_skills(self, sandbox_id, *, thread_id, user_id, projection) -> None:
        # The inherited no-op is documented for bind-mount providers; this is
        # a remote-pod provider, so declare the gap instead of silently
        # accepting it. The harness never reaches this method as configured:
        # supports_agent_skill_isolation stays False (the ABC default), so
        # middleware fails explicit-Agent-policy runs with its own error
        # before acquire — exactly like the shipped OpenSandbox provider,
        # which also offers no projection. An upload implementation cannot
        # land in this example either: the skills root (/mnt/skills) is
        # outside sandboxd's --root-dir=/mnt/user-data confinement and not
        # creatable by the non-root sandbox user. Anyone extending this
        # provider to advertise supports_agent_skill_isolation must project
        # the four SkillProjectionPaths category dirs into the pod (see the
        # e2b provider's upload-based sync_agent_skills for the reference
        # shape) — not merely remove this raise.
        raise NotImplementedError(
            "AgentSandboxProvider does not project Agent skill views into sandbox pods"
        )

    def release(self, sandbox_id) -> None:
        # Hold the acquire lock through teardown: popping under the lock but
        # tearing down outside it would let a concurrent acquire of the same
        # scope miss the cache, adopt the still-existing claim, and then have
        # that claim deleted by this teardown. Serializing teardown with cold
        # starts matches the trade-off acquire() already makes (it holds the
        # lock across create_sandbox).
        with self._lock:
            entry = self._sandboxes.pop(sandbox_id, None)
            if entry is None:
                return
            claim_name, sandbox = entry
            sdk_sandbox = sandbox._sbx
            try:
                # terminate() is idempotent and 404-tolerant, and orders
                # close-then-delete internally; delete_sandbox() would swallow
                # failures and report success either way.
                sdk_sandbox.terminate()
            except Exception as terminate_error:
                # Connection teardown failed; still destroy the claim
                # server-side (delete_sandbox_claim tolerates a 404).
                try:
                    sdk_sandbox.k8s_helper.delete_sandbox_claim(claim_name, sdk_sandbox.namespace)
                except Exception as delete_error:
                    raise RuntimeError(
                        f"failed to destroy agent-sandbox claim {claim_name} "
                        f"(terminate: {terminate_error}; delete: {delete_error})"
                    ) from delete_error
        logger.info("released agent-sandbox %s", sandbox_id)

    def reset(self) -> None:
        # Release everything so no claims are orphaned when DeerFlow resets
        # the provider singleton; one failing sandbox must not abort the loop
        # and strand the rest.
        for sandbox_id in list(self._sandboxes):
            try:
                self.release(sandbox_id)
            except Exception:
                logger.warning("failed to release agent-sandbox %s during reset", sandbox_id, exc_info=True)

    def shutdown(self) -> None:
        self.reset()
