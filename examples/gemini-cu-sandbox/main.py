# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law of agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import subprocess
import shlex
import logging
import os

from fastapi import FastAPI
from pydantic import BaseModel

class AgentQuery(BaseModel):
    """Request model for the /agent endpoint."""
    query: str
    api_key: str | None = None # Allow explicit API key for testing

class AgentResponse(BaseModel):
    """Response model for the /agent endpoint."""
    stdout: str
    stderr: str
    exit_code: int

app = FastAPI(
    title="Agentic Sandbox Runtime for Computer Use",
    description="An API server for executing browser tasks using the computer-use-preview agent.",
    version="1.0.0",
)

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.post("/agent", summary="Run the browser agent with a query", response_model=AgentResponse)
def agent_command(request: AgentQuery):
    """
    Executes the computer-use-preview agent with a given query.
    """
    # Use a fresh environment for each subprocess call
    env = os.environ.copy()
    env["PLAYWRIGHT_HEADLESS"] = "1"

    # Prioritize API key from the request if provided.
    if request.api_key:
        env["GEMINI_API_KEY"] = request.api_key

    # If GEMINI_API_KEY is still not set (neither from request nor initial environment),
    # then return an error.
    if "GEMINI_API_KEY" not in env:
        return AgentResponse(
            stdout="",
            stderr="GEMINI_API_KEY not found in request or environment variables. Please set it via request or environment variable (e.g., K8s secret).",
            exit_code=1,
        )

    try:
        # The command to run the agent. Using shlex.quote for safety.
        command = f"python computer-use-preview/main.py --query {shlex.quote(request.query)}"
        
        # Execute the command
        args = shlex.split(command)
        process = subprocess.run(
            args,
            capture_output=True,
            text=True,
            cwd="/app",
            env=env
        )
        return AgentResponse(
            stdout=process.stdout,
            stderr=process.stderr,
            exit_code=process.returncode
        )
    except Exception as e:
        logging.exception("An error occurred during agent execution.")
        return AgentResponse(
                    stdout="",
                    stderr=f"Failed to execute command: {str(e)}",
                    exit_code=1
         )