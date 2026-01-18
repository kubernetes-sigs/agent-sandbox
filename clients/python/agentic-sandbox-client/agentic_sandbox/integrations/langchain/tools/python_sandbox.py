from .tool import sandbox_tool


def create_python_sandbox_tool(sandbox_settings, description=None):
    """
    Create Langchain tool that runs Python code inside Agent Sandbox

    Args:
        sandbox_settings: Settings to create a sandbox.
        description: Tool description.

    """
    return sandbox_tool(sandbox_settings, description)(execute_python_code_in_sandbox)


def execute_python_code_in_sandbox(code: str, **kwargs) -> dict:
    """
    Executes the code in a sandbox and returns execution results.

    Args:
        code: Python code to execute.

    Returns:
        Dictionary with following fields:
        - status: The execution status.
        - stdout: Stdout of the executed code.
        - stderr: Stderr of the executed code.
        - exit_code: Exit code of the executed process.

    """
    sandbox_settings = kwargs["sandbox"]

    with sandbox_settings.create_client() as sandbox:
        sandbox.write("main.py", code)
        result = sandbox.run("python3 main.py")

    return {
        "status": "success",
        "stdout": result.stdout,
        "stderr": result.stderr,
        "exit_code": result.exit_code,
    }
