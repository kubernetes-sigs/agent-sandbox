from agentic_sandbox.sandbox_client import ExecutionResult
from agentic_sandbox.integrations.langchain.tools.python_sandbox import (
    create_python_sandbox_tool,
)


def test_python_sandbox_tool(
    sandbox_settings_mock, sandbox_client_mock, sandbox_execution_result
):

    sandbox_client_mock.run.return_value = sandbox_execution_result
    sandbox_settings_mock.create_client.return_value = sandbox_client_mock

    tool = create_python_sandbox_tool(sandbox_settings_mock)

    result = tool.invoke("some code")

    sandbox_client_mock.write.assert_called_with("main.py", "some code")
    assert result == {
        "status": "success",
        "stdout": sandbox_execution_result.stdout,
        "stderr": sandbox_execution_result.stderr,
        "exit_code": sandbox_execution_result.exit_code,
    }
