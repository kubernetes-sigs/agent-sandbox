import shlex
import subprocess
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

class ExecuteRequest(BaseModel):
    command: str

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
        # Return the exact schema the SDK expects
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exitCode": result.returncode
        }
    except Exception as e:
        return {
            "stdout": "",
            "stderr": str(e),
            "exitCode": 1
        }
