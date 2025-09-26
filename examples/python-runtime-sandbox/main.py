import subprocess
import os
import shlex
import logging

from fastapi import FastAPI, UploadFile, File
from fastapi.responses import FileResponse, JSONResponse
from pydantic import BaseModel

class ExecuteRequest(BaseModel):
    """Request model for the /execute endpoint."""
    command: str

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