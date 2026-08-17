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

import os
import shlex
import subprocess

from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

ALLOWED_COMMANDS = {"ls", "echo", "cat", "grep", "pwd", "zip", "unzip", "mv", "curl"}
WORKING_DIR = "/app" if os.path.isdir("/app") else os.getcwd()

class ExecuteRequest(BaseModel):
    command: str


class ExecuteResponse(BaseModel):
    """Response model for the /execute endpoint."""
    stdout: str
    stderr: str
    exit_code: int

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}


@app.post("/execute")
def execute_command(req: ExecuteRequest):
    """
    Executes a shell command inside the sandbox and returns its output.
    Uses shlex.split for security to prevent shell injection.
    """
    try:
        # Syntax Validation: shlex.split raises ValueError on malformed quotes
        try:
            args = shlex.split(req.command)
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
