import os
import posixpath
import shlex
import subprocess
from datetime import datetime, timezone
from typing import Literal

from fastapi import FastAPI, File, HTTPException, UploadFile
from fastapi.responses import Response
from pydantic import BaseModel

app = FastAPI()

SANDBOX_ROOT = os.getcwd()

class ExecuteRequest(BaseModel):
    command: str


class FileEntry(BaseModel):
    """Represents a file or directory entry in the sandbox."""
    name: str # Name of the file.
    size: int  # Size of the file in bytes.
    type: Literal["file", "directory"]  # Type of the entry (file or directory).
    mod_time: float # Last modification time of the file. (POSIX timestamp)


def _resolve_safe_path(rel_path: str) -> str:
    if any(ord(c) < 0x20 or ord(c) == 0x7F for c in rel_path):
        raise HTTPException(status_code=400, detail="Path contains control characters.")

    stripped = rel_path.strip()
    if not stripped:
        raise HTTPException(status_code=400, detail="Path cannot be empty.")

    normalized = posixpath.normpath(stripped).lstrip("/")
    if not normalized or normalized == ".":
        raise HTTPException(status_code=400, detail="Path does not name a file.")

    parts = normalized.split("/")
    if any(part == ".." for part in parts):
        raise HTTPException(status_code=400, detail="Path escapes the sandbox root.")

    root = os.path.realpath(SANDBOX_ROOT)
    full_path = os.path.realpath(os.path.join(root, normalized))
    print(full_path)
    if full_path != root and not full_path.startswith(root + os.sep):
        raise HTTPException(status_code=400, detail="Path escapes the sandbox root.")
    print(full_path)
    return full_path


@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}


@app.post("/execute")
def execute_command(req: ExecuteRequest):
    try:
        args = shlex.split(req.command)
        result = subprocess.run(
            args,
            capture_output=True,
            text=True,
            timeout=120
        )
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exit_code": result.returncode
        }
    except Exception as e:
        return {
            "stdout": "",
            "stderr": str(e),
            "exit_code": 1
        }


@app.post("/upload", summary="Upload a file into the sandbox")
async def upload_file(file: UploadFile = File(...)):
    # The SDK sends the destination path as the multipart "filename" field.
    if not file.filename:
        raise HTTPException(status_code=400, detail="Missing filename.")

    dest_path = _resolve_safe_path(file.filename)
    os.makedirs(os.path.dirname(dest_path), exist_ok=True)

    content = await file.read()
    with open(dest_path, "wb") as f:
        f.write(content)

    return {"status": "ok", "path": os.path.relpath(dest_path, os.path.realpath(SANDBOX_ROOT))}


@app.get("/download/{path:path}", summary="Download a file from the sandbox")
async def download_file(path: str):
    full_path = _resolve_safe_path(path)

    if not os.path.isfile(full_path):
        raise HTTPException(status_code=404, detail=f"File not found: {path}")

    with open(full_path, "rb") as f:
        content = f.read()

    return Response(content=content, media_type="application/octet-stream")


@app.get("/list/{path:path}", summary="List directory contents in the sandbox")
async def list_dir(path: str):
    full_path = _resolve_safe_path(path)

    if not os.path.isdir(full_path):
        raise HTTPException(status_code=404, detail=f"Directory not found: {path}")

    entries = []
    for name in sorted(os.listdir(full_path)):
        entry_path = os.path.join(full_path, name)
        stat = os.stat(entry_path)
        entries.append(
            FileEntry(
                name=name,
                type="directory" if os.path.isdir(entry_path) else "file",
                size=stat.st_size,
                mod_time=datetime.fromtimestamp(
                    stat.st_mtime, tz=timezone.utc
                ).timestamp(),
            )
        )

    return [e.model_dump() for e in entries]


@app.get("/exists/{path:path}", summary="Check whether a path exists in the sandbox")
async def path_exists(path: str):
    full_path = _resolve_safe_path(path)
    return {"exists": os.path.exists(full_path)}
