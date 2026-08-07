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
        result = subprocess.run(
            req.command,
            shell=True,
            capture_output=True,
            text=True,
            executable="/bin/bash",
            timeout=60
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
            "exit_code": -1,
        }
