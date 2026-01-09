from unittest import mock

import pytest

from agentic_sandbox.sandbox_client import ExecutionResult


@pytest.fixture()
def sandbox_client_mock():
    sandbox_mock = mock.MagicMock()
    sandbox_mock.__enter__.return_value = sandbox_mock
    sandbox_mock.write = mock.MagicMock()

    return sandbox_mock


@pytest.fixture()
def sandbox_execution_result():
    return ExecutionResult(
        stdout="some output",
        stderr="some logs",
        exit_code=0,
    )


@pytest.fixture()
def sandbox_settings_mock():

    settings = mock.MagicMock()

    return settings
