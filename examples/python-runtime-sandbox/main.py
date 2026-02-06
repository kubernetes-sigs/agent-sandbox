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
import asyncio

from fastapi import FastAPI, UploadFile, File
from jupyter_client import AsyncKernelManager
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel

class ExecuteRequest(BaseModel):
    """Request model for the /execute endpoint."""
    command: str

class ExecuteStatefulRequest(BaseModel):
    """Request model for the /execute_stateful endpoint."""
    code: str
    language: str = "python"
    timeout: int = 10  # Default timeout in seconds

class ExecuteResponse(BaseModel):
    """Response model for the /execute endpoint."""
    stdout: str
    stderr: str
    exit_code: int

app = FastAPI(
    title="Agentic Sandbox Runtime",
    description="An API server for executing commands and managing files in a secure sandbox.",
    version="1.0.0",
)

# Global state to manage the IPython kernel
kernel_manager = AsyncKernelManager(kernel_name='python3')
kernel_client = None
kernel_lock = asyncio.Lock()

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.on_event("startup")
async def start_kernel():
    """Starts the IPython kernel when the sandbox starts."""
    global kernel_client
    await kernel_manager.start_kernel()
    kernel_client = kernel_manager.client()
    kernel_client.start_channels()
    
    # Change the working directory to /app to ensure all code execution happens in the correct context
    kernel_client.execute("import os, pickle; os.chdir('/app')")

@app.on_event("shutdown")
async def shutdown_kernel():
    """Cleans up the IPython kernel when the sandbox stops."""
    if kernel_client:
        kernel_client.stop_channels()
    if kernel_manager:
        await kernel_manager.shutdown_kernel()
    
@app.post("/execute", summary="Execute a shell command", response_model=ExecuteResponse)
async def execute_command(request: ExecuteRequest):
    """
    Executes a shell command inside the sandbox and returns its output.
    Uses shlex.split for security to prevent shell injection.
    """
    try:
        # Split the command string into a list to safely pass to subprocess
        args = shlex.split(request.command)
        
        # Execute the command, always from the /app directory
        process = subprocess.run(
            args,
            capture_output=True,
            text=True,
            cwd="/app" 
        )
        return ExecuteResponse(
            stdout=process.stdout,
            stderr=process.stderr,
            exit_code=process.returncode
        )
    except Exception as e:
        return ExecuteResponse(
            stdout="",
            stderr=f"Failed to execute command: {str(e)}",
            exit_code=1
        )

@app.post("/execute_command_stateful", summary="Execute code in a stateful way.", response_model=ExecuteResponse)
async def execute_command_stateful(request: ExecuteStatefulRequest):
    if not kernel_client:
        return ExecuteResponse(
            stdout="",
            stderr="IPython kernel is not initialized.",
            exit_code=1
        )

    # Ensure that only one execution happens at a time
    async with kernel_lock:
        # 1. Send code to the kernel
        msg_id = kernel_client.execute(request.code)
        
        stdout = []
        stderr = []
        
        # 2. Listen for output on the IOPub channel
        while True:
            try:
                # Poll for messages with the request-specific timeout
                msg = await asyncio.wait_for(kernel_client.get_iopub_msg(), timeout=request.timeout)
                
                # Ignore messages from previous timed-out executions
                if msg['parent_header'].get('msg_id') != msg_id:
                    continue

                msg_type = msg['header']['msg_type']
                content = msg['content']

                if msg_type == 'stream':
                    if content['name'] == 'stdout':
                        stdout.append(content['text'])
                    else:
                        stderr.append(content['text'])
                
                elif msg_type == 'execute_result':
                    # Captures the value of the last line of code (e.g. "x + 5")
                    stdout.append(content['data'].get('text/plain', ''))

                elif msg_type == 'error':
                    # Captures Python Tracebacks if the code fails
                    stderr.append("\n".join(content['traceback']))

                # 'idle' status indicates the kernel has finished execution
                if msg_type == 'status' and content['execution_state'] == 'idle':
                    break
                    
            except asyncio.TimeoutError:
                # Stop if we haven't heard from the kernel in 10 seconds
                stderr.append(f"Execution timed out after {request.timeout} seconds due to Kernel inactivity.")
                await kernel_manager.interrupt_kernel()
                break

    return {
        "stdout": "".join(stdout),
        "stderr": "".join(stderr),
        "exit_code": 0 if not stderr else 1
    }
    
@app.post("/upload", summary="Upload a file to the sandbox")
async def upload_file(file: UploadFile = File(...)):
    """
    Receives a file and saves it to the /app directory in the sandbox.
    """
    try:
        logging.info(f"--- UPLOAD_FILE CALLED: Attempting to save '{file.filename}' ---")
        file_path = os.path.join("/app", file.filename)
        
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
    Downloads a specified file from the /app directory in the sandbox.
    """
    full_path = os.path.join("/app", file_path)
    if os.path.isfile(full_path):
        return FileResponse(path=full_path, media_type='application/octet-stream', filename=file_path)
    return JSONResponse(status_code=404, content={"message": "File not found"})