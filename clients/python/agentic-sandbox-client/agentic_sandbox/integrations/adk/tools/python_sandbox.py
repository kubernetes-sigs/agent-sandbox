from .base import PredefinedSandboxFunctionTool


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


class PythonSandboxTool(PredefinedSandboxFunctionTool):
    func = execute_python_code_in_sandbox
