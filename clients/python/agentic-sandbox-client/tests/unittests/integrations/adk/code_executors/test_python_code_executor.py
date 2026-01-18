from unittest import mock

from google.adk.code_executors.code_execution_utils import (
    CodeExecutionInput,
    CodeExecutionResult,
)

from agentic_sandbox.integrations.adk.code_executors.python_sandbox import (
    PythonSandboxCodeExecutor,
)


def test_code_executor(
    sandbox_settings_mock, sandbox_client_mock, sandbox_execution_result
):

    sandbox_client_mock.run.return_value = sandbox_execution_result
    sandbox_settings_mock.create_client.return_value = sandbox_client_mock

    executor = PythonSandboxCodeExecutor(sandbox_settings_mock)

    mock_invocation_context = mock.MagicMock()
    result = executor.execute_code(
        mock_invocation_context, CodeExecutionInput(code="some code")
    )

    sandbox_client_mock.write.assert_called_with("main.py", "some code")
    assert result == CodeExecutionResult(
        stdout=sandbox_execution_result.stdout,
        stderr=sandbox_execution_result.stderr,
        output_files=[],
    )
