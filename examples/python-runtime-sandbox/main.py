# Copyright 2025 The Kubernetes Authors.
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

import asyncio
import math
import signal
import subprocess
import os
import logging

from fastapi import FastAPI, UploadFile, File
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel, Field

# Exit code reported when a command is killed for exceeding its timeout. It
# follows GNU coreutils `timeout` rather than reporting the SIGKILL status,
# which a cgroup OOM kill would also produce.
TIMEOUT_EXIT_CODE = 124

class ExecuteRequest(BaseModel):
    """Request model for the /execute endpoint."""
    command: str
    # Optional per-request limit. It can only shorten the server-wide
    # SANDBOX_EXEC_TIMEOUT_SECONDS, never extend it, so the operator's limit
    # stays the upper bound for every caller.
    timeout_seconds: float | None = Field(default=None, gt=0, allow_inf_nan=False)

class ExecuteResponse(BaseModel):
    """Response model for the /execute endpoint."""
    stdout: str
    stderr: str
    exit_code: int
    timed_out: bool = False

def get_base_dir() -> str:
    """Reads SANDBOX_BASE_DIR, falling back to /app when it's unset or blank.

    Making the base directory configurable lets the sandbox run with a
    read-only root filesystem: the runtime code stays wherever the image put
    it, while commands and file operations are confined to a writable volume
    (e.g. an emptyDir) mounted at SANDBOX_BASE_DIR.
    """
    return os.environ.get("SANDBOX_BASE_DIR", "").strip() or "/app"

def get_safe_path(file_path: str) -> str:
    """Sanitizes the file path to ensure it stays within the base directory."""
    base_dir = os.path.realpath(get_base_dir())
    # Remove leading slashes to ensure path is relative
    clean_path = file_path.lstrip("/")
    full_path = os.path.realpath(os.path.join(base_dir, clean_path))

    if os.path.commonpath([base_dir, full_path]) != base_dir:
        raise ValueError("Access denied: Path must be within the sandbox base directory")

    return full_path

app = FastAPI(
    title="Agentic Sandbox Runtime",
    description="An API server for executing commands and managing files in a secure sandbox.",
    version="1.0.0",
)

def _get_exec_timeout_seconds() -> float:
    """Reads SANDBOX_EXEC_TIMEOUT_SECONDS, falling back to the 300s default
    (with a warning) if it's unset or not a finite number greater than 0, so
    a misconfigured value doesn't fail every /execute request.
    """
    raw = os.environ.get("SANDBOX_EXEC_TIMEOUT_SECONDS", "300")
    try:
        value = float(raw)
    except ValueError:
        value = float("nan")

    if not (math.isfinite(value) and value > 0):
        logging.warning(
            "Ignoring invalid SANDBOX_EXEC_TIMEOUT_SECONDS=%r; using default of 300 seconds",
            raw,
        )
        return 300.0
    return value

def _run_command(args: list, timeout: float) -> subprocess.CompletedProcess:
    """Runs args as the leader of a new process group so that on timeout the
    entire process tree can be killed, not just the direct child (matching
    the pattern used in examples/firecracker-sandbox/main.py).
    """
    with subprocess.Popen(
        args,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        cwd=get_base_dir(),
        start_new_session=True,
    ) as process:
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(os.getpgid(process.pid), signal.SIGKILL)
            except (ProcessLookupError, OSError):
                pass
            stdout, stderr = process.communicate()
            raise subprocess.TimeoutExpired(args, timeout, output=stdout, stderr=stderr)
        return subprocess.CompletedProcess(args, process.returncode, stdout, stderr)

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.post("/execute", summary="Execute a shell command", response_model=ExecuteResponse)
async def execute_command(request: ExecuteRequest):
    """
    Executes a shell command inside the sandbox and returns its output.

    The command runs under "/bin/sh -c" so shell syntax the caller expects
    to work (&&, |, >, ;, quoting) actually does. Without a shell, operators
    like these are passed as literal argv to the first command instead of
    being interpreted, which does not fail loudly: e.g. "mkdir -p a && echo
    hi > a/f.txt" makes mkdir create directories literally named "&&",
    "echo", "hi", ">" and "a/f.txt". The caller already has arbitrary code
    execution in their own sandbox, so a shell adds no new exposure.
    """
    try:
        args = ["/bin/sh", "-c", request.command]

        # Execute the command, always from the base directory. Run it in a
        # worker thread so a long-running or hung command doesn't block the
        # event loop (and with it, the health check and file endpoints), and
        # enforce a timeout so a runaway command can't wedge the sandbox
        # forever.
        timeout = _get_exec_timeout_seconds()
        if request.timeout_seconds is not None:
            timeout = min(timeout, request.timeout_seconds)
        process = await asyncio.to_thread(_run_command, args, timeout)
        return ExecuteResponse(
            stdout=process.stdout,
            stderr=process.stderr,
            exit_code=process.returncode
        )
    except subprocess.TimeoutExpired as e:
        # Report the timeout as data rather than a generic failure, so callers
        # can tell it apart from a command that exited non-zero on its own,
        # and keep whatever output the command produced before it was killed.
        stderr = e.stderr or ""
        if stderr and not stderr.endswith("\n"):
            stderr += "\n"
        return ExecuteResponse(
            stdout=e.output or "",
            stderr=f"{stderr}Command timed out after {e.timeout:g} seconds",
            exit_code=TIMEOUT_EXIT_CODE,
            timed_out=True,
        )
    except Exception as e:
        return ExecuteResponse(
            stdout="",
            stderr=f"Failed to execute command: {str(e)}",
            exit_code=1
        )

@app.post("/upload", summary="Upload a file to the sandbox")
async def upload_file(file: UploadFile = File(...)):
    """
    Receives a file and saves it to the base directory in the sandbox.
    """
    try:
        logging.info(f"--- UPLOAD_FILE CALLED: Attempting to save '{file.filename}' ---")

        try:
            file_path = get_safe_path(file.filename)
        except ValueError:
            return JSONResponse(
                status_code=403,
                content={"message": "Access denied"}
            )

        # The filename may carry a relative destination path (e.g.
        # "data/input.csv"); create the intermediate directories so such
        # uploads don't fail. file_path is already confined to the base
        # directory by get_safe_path, so its parents are too.
        os.makedirs(os.path.dirname(file_path), exist_ok=True)

        with open(file_path, "wb") as f:
            f.write(await file.read())
            
        return JSONResponse(
            status_code=200,
            content={"message": f"File '{file.filename}' uploaded successfully."}
        )
    except Exception as e:
        logging.exception("An error occurred during file upload.") 
        return JSONResponse(
            status_code=500,
            content={"message": f"File upload failed: {str(e)}"}
        )

@app.get("/download/{file_path:path}", summary="Download a file from the sandbox")
async def download_file(file_path: str):
    """
    Downloads a specified file from the base directory in the sandbox.
    """
    try:
        full_path: str = get_safe_path(file_path)
    except ValueError:
        return JSONResponse(status_code=403, content={"message": "Access denied"})

    if os.path.isfile(full_path):
        return FileResponse(
            path=full_path,
            media_type='application/octet-stream',
            filename=os.path.basename(full_path),
        )
    return JSONResponse(status_code=404, content={"message": "File not found"})

@app.get("/list/{file_path:path}", summary="List files in a directory")
async def list_files(file_path: str):
    """
    Lists the contents of a directory under the base directory in the sandbox.
    """
    try:
        full_path: str = get_safe_path(file_path)
    except ValueError:
        return JSONResponse(status_code=403, content={"message": "Access denied"})

    if not os.path.isdir(full_path):
        return JSONResponse(status_code=404, content={"message": "Path is not a directory"})
    
    try:
        entries = []
        with os.scandir(full_path) as it:
            for entry in it:
                stats = entry.stat()
                entries.append({
                    "name": entry.name,
                    "size": stats.st_size,
                    "type": "directory" if entry.is_dir() else "file",
                    "mod_time": stats.st_mtime
                })
        return JSONResponse(status_code=200, content=entries)
    except Exception as e:
        return JSONResponse(status_code=500, content={"message": f"List files failed: {str(e)}"})

@app.get("/exists/{file_path:path}", summary="Check if the relative path exists")
async def exists(file_path: str):
    """
    Checks if a specified file or directory exists under the base directory in the sandbox.
    """
    try:
        full_path: str = get_safe_path(file_path)
    except ValueError:
        return JSONResponse(status_code=403, content={"message": "Access denied"})

    return JSONResponse(status_code=200, content={
        "path": file_path,
        "exists": os.path.exists(full_path)
    })
