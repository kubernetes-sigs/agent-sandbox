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

import subprocess
import os
import shlex
import logging
import shutil
from pathlib import Path

from fastapi import FastAPI, UploadFile, File
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel

class ExecuteRequest(BaseModel):
    """Request model for the /execute endpoint."""
    command: str

class PythonExecuteRequest(BaseModel):
    """Request model for the /execute-python endpoint."""
    code: str

class ExecuteResponse(BaseModel):
    """Response model for the /execute endpoint."""
    stdout: str
    stderr: str
    exit_code: int


ALLOWED_COMMANDS = {"ls", "echo", "cat", "grep", "pwd", "zip", "unzip", "mv", "curl"}

WORKING_DIR = "/app" if os.path.isdir("/app") else os.getcwd()

app = FastAPI(
    title="Agentic Sandbox Runtime",
    description="An API server for executing commands and managing files in a secure sandbox.",
    version="1.0.0",
)

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.post("/execute", summary="Execute a shell command", response_model=ExecuteResponse)
async def execute_command(request: ExecuteRequest):
    """
    Executes a shell command inside the sandbox and returns its output.
    Uses shlex.split for security to prevent shell injection.
    """
    try:
        # Syntax Validation: shlex.split raises ValueError on malformed quotes
        try:
            args = shlex.split(request.command)
        except ValueError as e:
            return ExecuteResponse(
                stdout="",
                stderr=f"Malformed command syntax: {str(e)}",
                exit_code=1
            )
        # Structural Validation: Ensure the command isn't empty
        if not args:
            return ExecuteResponse(
                stdout="",
                stderr="No command provided",
                exit_code=1
            )

        # Security Validation: Check against an Allow-list
        executable = args[0]
        if executable not in ALLOWED_COMMANDS:
            return ExecuteResponse(
                stdout="",
                stderr=f"Forbidden command: '{executable}'. Only {list(ALLOWED_COMMANDS)} are allowed.",
                exit_code=1
            )

        # Execute the command, always from the WORKING_DIR directory
        process = subprocess.run(
            args,
            capture_output=True,
            text=True,
            cwd=WORKING_DIR,
            timeout=30,
        )
        return ExecuteResponse(
            stdout=process.stdout,
            stderr=process.stderr,
            exit_code=process.returncode
        )
    except subprocess.TimeoutExpired:
        return ExecuteResponse(stdout="", stderr="Command timed out", exit_code=124)
    except Exception as e:
        return ExecuteResponse(stdout="", stderr=str(e), exit_code=1)

@app.post("/execute-python", summary="Execute Python code", response_model=ExecuteResponse)
async def execute_python(request: PythonExecuteRequest):
    """
    Executes arbitrary Python code inside the sandbox and returns its output.
    """
    try:
        python_exe = shutil.which("python3") or shutil.which("python")
        if not python_exe:
            return ExecuteResponse(
                stdout="",
                stderr="Python interpreter not found",
                exit_code=1
            )
        process = subprocess.run(
            [python_exe, "-c", request.code],
            capture_output=True,
            text=True,
            cwd=WORKING_DIR,
            timeout=30,
        )
        return ExecuteResponse(
            stdout=process.stdout,
            stderr=process.stderr,
            exit_code=process.returncode
        )
    except subprocess.TimeoutExpired:
        return ExecuteResponse(stdout="", stderr="Command timed out", exit_code=124)
    except Exception as e:
        return ExecuteResponse(stdout="", stderr=str(e), exit_code=1)

@app.post("/upload", summary="Upload a file to the sandbox")
async def upload_file(file: UploadFile = File(...)):
    """
    Receives a file and saves it to the working directory in the sandbox.
    """
    try:
        logging.info(f"--- UPLOAD_FILE CALLED: Attempting to save '{file.filename}' ---")
        working_dir = Path(WORKING_DIR).resolve()
        full_path = (working_dir / file.filename).resolve()
        if not full_path.is_relative_to(working_dir) or full_path == working_dir:
            return JSONResponse(status_code=400, content={"message": "Invalid file path"})

        full_path.parent.mkdir(parents=True, exist_ok=True)
        with open(full_path, "wb") as f:
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
    Downloads a specified file from the working directory in the sandbox.
    """
    working_dir = Path(WORKING_DIR).resolve()
    full_path = (working_dir / file_path).resolve()
    if not full_path.is_relative_to(working_dir) or full_path == working_dir:
        return JSONResponse(status_code=400, content={"message": "Invalid file path"})

    if full_path.is_file():
        return FileResponse(path=str(full_path), media_type='application/octet-stream', filename=os.path.basename(file_path))
    return JSONResponse(status_code=404, content={"message": "File not found"})
