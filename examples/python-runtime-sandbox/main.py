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

class ExecuteCodeRequest(BaseModel):
    """Request model for the /execute_code endpoint."""
    code: str
    language: str = "python"
    timeout: int = 10  # Default timeout in seconds
    

class ExecuteResponse(BaseModel):
    """Response model for the /execute endpoint."""
    stdout: str
    stderr: str
    exit_code: int
    
class ExecuteCodeResponse(BaseModel): 
    """Response model for the /execute_code endpoint."""
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

async def _start_kernel_internal():
    """Helper to start or restart the kernel."""
    global kernel_client
    if kernel_manager.has_kernel:
        await kernel_manager.shutdown_kernel(now=True)
        
    await kernel_manager.start_kernel()
    kernel_client = kernel_manager.client()
    kernel_client.start_channels()
    # Change the working directory to /app to ensure all code execution happens in the correct context
    kernel_client.execute("import os, pickle; os.chdir('/app')")

@app.on_event("startup")
async def start_kernel():
    """Starts the IPython kernel when the sandbox starts."""
    await _start_kernel_internal()

@app.on_event("shutdown")
async def shutdown_kernel():
    """Cleans up the IPython kernel when the sandbox stops."""
    if kernel_client:
        kernel_client.stop_channels()
    if kernel_manager:
        await kernel_manager.shutdown_kernel()
    
async def interrupt_and_drain():
    """Interrupts the kernel and drains the IOPub channel until idle."""
    if not kernel_manager or not kernel_client:
        return
    
    try:
        await kernel_manager.interrupt_kernel()
        while True:
            # Use a short timeout to avoid hanging forever if the kernel is dead
            msg = await asyncio.wait_for(kernel_client.get_iopub_msg(), timeout=1.0)
            if msg['header']['msg_type'] == 'status' and msg['content']['execution_state'] == 'idle':
                break
    except (asyncio.TimeoutError, Exception):
        logging.warning("Kernel interruption or drain failed. Forcing kernel restart.")
        await _start_kernel_internal()

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

@app.post("/execute_code", summary="Execute code in a stateful way.", response_model=ExecuteCodeResponse)
async def execute_code(request: ExecuteCodeRequest):
    if request.language != "python":
        return ExecuteCodeResponse(stdout="", stderr=f"Unsupported language: {request.language}", exit_code=1)
    
    # 1. Self-healing check: If client is missing or dead, try to start it
    async with kernel_lock:
        if not kernel_client or not kernel_manager.has_kernel:
            logging.warning("Kernel not initialized. Attempting to start...")
            await _start_kernel_internal()

        # 2. Check Liveness
        is_alive = False
        try:
            check = kernel_manager.is_alive()
            is_alive = await check if asyncio.iscoroutine(check) else check
        except Exception:
            pass
        
        if not is_alive:
            logging.warning("Kernel is dead. Restarting...")
            await _start_kernel_internal()

        # 3. Flush the IOPub channel before starting
        # This prevents "ghost" output from previous timed-out runs
        try:
            while True:
                await asyncio.wait_for(kernel_client.get_iopub_msg(), timeout=0.01)
        except (asyncio.TimeoutError, Exception):
            pass

        # 4. Execute the requested code.
        msg_id = kernel_client.execute(request.code)
        stdout, stderr = [], []
        exit_code = 0

        async def collect_io():
            nonlocal exit_code
            while True:
                # We listen to IOPub for the "Stream" of output
                msg = await kernel_client.get_iopub_msg()
                if msg.get('parent_header', {}).get('msg_id') != msg_id:
                    continue

                msg_type = msg['header']['msg_type']
                content = msg['content']

                if msg_type == 'stream':
                    if content['name'] == 'stdout':
                        stdout.append(content['text'])
                    else:
                        stderr.append(content['text'])
                elif msg_type in ('execute_result', 'display_data'):
                    # Capture rich output/returned values
                    data = content['data'].get('text/plain', '')
                    stdout.append(data)
                elif msg_type == 'error':
                    stderr.append("\n".join(content['traceback']))
                elif msg_type == 'status' and content['execution_state'] == 'idle':
                    break

        try:
            # 5. Wait for IO and the shell reply
            await asyncio.wait_for(collect_io(), timeout=request.timeout)
            
            # 6. Get the actual exit status from the shell channel
            shell_msg = await asyncio.wait_for(kernel_client.get_shell_msg(), timeout=1.0)
            if shell_msg['content'].get('status') == 'error':
                exit_code = 1
            
            return ExecuteCodeResponse(
                stdout="".join(stdout),
                stderr="".join(stderr),
                exit_code=exit_code
            )

        except asyncio.TimeoutError:
            await interrupt_and_drain()
            return ExecuteCodeResponse(
                stdout="".join(stdout),
                stderr="".join(stderr) + f"\n[Timeout after {request.timeout}s]",
                exit_code=130
            )
    
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