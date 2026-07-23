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

"""
Sandbox runtime for agent-sandbox.

A lightweight HTTP API exposed inside every Firecracker sandbox pod. Clients
(the verification script, the ``k8s_agent_sandbox`` Python SDK, or any HTTP
tool) call these endpoints to drive the sandbox:

  GET  /health       -> 204
  GET  /metrics      -> cgroup CPU / memory / IO stats
  POST /init         -> accept env vars / timestamp sync
  GET  /envs         -> current environment variables
  GET  /files        -> download a file  (?path=...)
  POST /files        -> upload a file    (multipart or JSON)
  POST /exec         -> execute a shell command -> {stdout, stderr, exit_code}

The server is intentionally small — it only covers the endpoints exercised by
the example's test client.

.. note::

    This API has **no in-process authentication**. Port 8888 must only be
    reachable through ``kubectl port-forward`` or ``sandbox-router``
    (``X-Sandbox-ID`` header) and must **never** be published directly to a
    public or untrusted network.
"""

import os
import shutil
import signal
import subprocess
import time
from pathlib import Path
from typing import Dict, List, Optional

from fastapi import FastAPI, HTTPException, UploadFile, File, Form
from fastapi.responses import FileResponse, JSONResponse, Response
from pydantic import BaseModel, Field

WORKSPACE = Path(os.environ.get("SANDBOX_WORKSPACE", "/workspace"))
WORKSPACE.mkdir(parents=True, exist_ok=True)

# Custom env vars set via /init are stored here and merged into /envs responses.
_user_env: Dict[str, str] = {}


# ---------------------------------------------------------------------------
# Request / response models
# ---------------------------------------------------------------------------
class ExecRequest(BaseModel):
    """Request body for POST /exec."""
    cmd: str
    args: Optional[List[str]] = None
    env: Optional[Dict[str, str]] = None
    cwd: Optional[str] = None
    timeout: float = Field(default=30.0, gt=0, le=300)


class ExecResponse(BaseModel):
    """Response body for POST /exec."""
    stdout: str
    stderr: str
    exit_code: int


class InitRequest(BaseModel):
    """Request body for POST /init (time + env sync)."""
    envs: Optional[Dict[str, str]] = None
    timestamp: Optional[float] = None


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def _safe_path(raw: str) -> Path:
    """Resolve *raw* against WORKSPACE and ensure it does not escape it."""
    if os.path.isabs(raw):
        candidate = Path(raw)
    else:
        candidate = (WORKSPACE / raw).resolve()
    resolved = candidate.resolve()
    try:
        resolved.relative_to(WORKSPACE.resolve())
    except ValueError as exc:
        raise HTTPException(status_code=403, detail="path escapes workspace") from exc
    return resolved


def _read_cgroup_file(*parts: str) -> Optional[str]:
    """Read a cgroup v2 file, returning None on any failure."""
    path = Path("/sys/fs/cgroup", *parts)
    try:
        return path.read_text().strip()
    except OSError:
        return None


def _collect_metrics() -> dict:
    """Collect a minimal set of cgroup v2 stats."""
    metrics: dict = {"timestamp": time.time()}

    cpu = _read_cgroup_file("cpu.stat")
    if cpu:
        cpu_stats: Dict[str, float] = {}
        for line in cpu.splitlines():
            parts = line.split()
            if len(parts) == 2:
                try:
                    cpu_stats[parts[0]] = float(parts[1])
                except ValueError:
                    pass
        metrics["cpu"] = cpu_stats

    mem = _read_cgroup_file("memory.current")
    if mem is not None:
        try:
            metrics["memory_bytes"] = int(mem)
        except ValueError:
            pass

    mem_peak = _read_cgroup_file("memory.peak")
    if mem_peak is not None:
        try:
            metrics["memory_peak_bytes"] = int(mem_peak)
        except ValueError:
            pass

    io = _read_cgroup_file("io.stat")
    if io:
        metrics["io"] = io.splitlines()

    return metrics


# ---------------------------------------------------------------------------
# App
# ---------------------------------------------------------------------------
app = FastAPI(
    title="Firecracker sandbox runtime",
    description="HTTP API exposed inside a Firecracker sandbox pod.",
    version="0.1.0",
)


@app.get("/health", summary="Health probe")
async def health() -> Response:
    """Return 204 No Content when the runtime is up."""
    return Response(status_code=204)


@app.get("/metrics", summary="Cgroup metrics")
def metrics() -> JSONResponse:
    """Return a snapshot of cgroup v2 stats for the sandbox process."""
    return JSONResponse(_collect_metrics())


@app.post("/init", summary="Initialize runtime (env + time sync)")
async def init(req: InitRequest) -> JSONResponse:
    """Accept env vars and an optional timestamp from the control-plane."""
    if req.envs:
        _user_env.update(req.envs)
        for key, value in req.envs.items():
            os.environ[key] = value
    server_time = time.time()
    skew = None
    if req.timestamp is not None:
        skew = server_time - req.timestamp
    return JSONResponse({"status": "ok", "server_time": server_time, "skew_seconds": skew})


@app.get("/envs", summary="List environment variables")
async def envs() -> JSONResponse:
    """Return the environment visible to the runtime.

    This intentionally includes the **full** process environment — not just
    the vars injected via ``/init`` — so callers can debug what the template
    and Kubernetes service discovery have provided. The transport-level
    no-auth warning in the module docstring applies: do not expose this
    endpoint to untrusted networks.
    """
    merged = {**os.environ, **_user_env}
    return JSONResponse(merged)


@app.post("/exec", summary="Execute a shell command", response_model=ExecResponse)
def exec_command(req: ExecRequest) -> ExecResponse:
    """Execute *cmd* (optionally with *args*) inside the sandbox workspace.

    stdout/stderr are captured and returned alongside the exit code.
    """
    cwd = _safe_path(req.cwd) if req.cwd else WORKSPACE
    if not cwd.is_dir():
        raise HTTPException(status_code=400, detail="cwd is not a directory")

    env = {**os.environ, **(req.env or {})}

    # If args are supplied, run them directly (no shell). Otherwise run cmd via
    # the user's shell, matching `sh -c` semantics.
    if req.args:
        argv = [req.cmd, *req.args]
        use_shell = False
    else:
        argv = req.cmd
        use_shell = True

    try:
        # Use Popen + start_new_session so we can kill the entire process
        # group on timeout. subprocess.run() only kills the direct child,
        # which with shell=True is `/bin/sh -c <cmd>` — its grandchildren
        # would otherwise leak as orphans.
        proc = subprocess.Popen(
            argv,
            cwd=str(cwd),
            env=env,
            shell=use_shell,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        try:
            stdout, stderr = proc.communicate(timeout=req.timeout)
        except subprocess.TimeoutExpired:
            pgid = os.getpgid(proc.pid)
            try:
                os.killpg(pgid, signal.SIGKILL)
            except (ProcessLookupError, OSError):
                pass
            stdout, stderr = proc.communicate()
            return ExecResponse(
                stdout=stdout or "",
                stderr=(stderr or "") + f"\n[timeout after {req.timeout}s]",
                exit_code=-1,
            )
        return ExecResponse(
            stdout=stdout,
            stderr=stderr,
            exit_code=proc.returncode,
        )
    except FileNotFoundError as exc:
        return ExecResponse(stdout="", stderr=str(exc), exit_code=127)
    except Exception as exc:  # noqa: BLE001
        return ExecResponse(stdout="", stderr=str(exc), exit_code=1)


@app.get("/files", summary="Download a file")
def download_file(path: str) -> FileResponse:
    """Download a file from the sandbox workspace."""
    target = _safe_path(path)
    if not target.is_file():
        raise HTTPException(status_code=404, detail="file not found")
    return FileResponse(target, filename=target.name)


@app.post("/files", summary="Upload a file")
def upload_file(
    path: str = Form(...),
    file: UploadFile = File(...),
) -> JSONResponse:
    """Upload a file into the sandbox workspace."""
    target = _safe_path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("wb") as fh:
        shutil.copyfileobj(file.file, fh)
    return JSONResponse({"path": str(target.relative_to(WORKSPACE)), "size": target.stat().st_size})


@app.get("/", summary="Root probe")
async def root() -> JSONResponse:
    return JSONResponse({"runtime": "firecracker-sandbox", "version": "0.1.0"})
