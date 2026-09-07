# Copyright 2026 The Kubernetes Authors.
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

"""Unit tests for coding_agent.py.

coding_agent.py imports torch and transformers at module level, but only
ever uses them inside CodeGenerationLLM (model loading and inference),
which these tests deliberately never instantiate for real -- LLM and code
executor collaborators are mocked throughout, the same way subprocess.run
is mocked in the other sandbox runtime tests. Lightweight stand-in modules
are injected into sys.modules before import so the module-level imports
succeed without installing the real (large, slow) torch/transformers
packages.
"""

import asyncio
import sys
import types
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

_fake_torch = types.ModuleType("torch")
_fake_torch.float16 = object()
_fake_torch.float32 = object()
_fake_torch.cuda = types.SimpleNamespace(is_available=lambda: False)
sys.modules["torch"] = _fake_torch

_fake_transformers = types.ModuleType("transformers")
_fake_transformers.AutoTokenizer = object
_fake_transformers.AutoModelForCausalLM = object
sys.modules["transformers"] = _fake_transformers

import coding_agent  # noqa: E402  (must follow the sys.modules stubs above)


def make_state(**overrides):
    state = {
        "user_request": "write a hello world script",
        "generated_code": "",
        "execution_result": "",
        "error_message": None,
        "iteration_count": 0,
        "max_iterations": 3,
        "status": "planning",
    }
    state.update(overrides)
    return state


# ---- CodeGenerationLLM._clean_code ----

def test_clean_code_leaves_plain_code_unchanged():
    code = "x = 1\nprint(x)"
    assert coding_agent.CodeGenerationLLM._clean_code(code) == code


def test_clean_code_strips_python_fence():
    code = "```python\nx = 1\nprint(x)\n```"
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "x = 1\nprint(x)"


def test_clean_code_strips_generic_fence():
    code = "```\nx = 1\n```"
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "x = 1"


def test_clean_code_truncates_at_note():
    code = "x = 1\nprint(x)\nNote: this prints 1."
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "x = 1\nprint(x)"


@pytest.mark.parametrize("prefix", ["Explanation:", "This code", "The code", "Output:"])
def test_clean_code_truncates_at_trailing_prose(prefix):
    code = f"x = 1\nprint(x)\n{prefix} does something."
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "x = 1\nprint(x)"


def test_clean_code_truncates_after_three_repeated_short_lines():
    # Lines under 3 chars that repeat more than twice in a row (a known
    # generation-loop failure mode) get cut off, along with everything
    # after them.
    code = "x = 1\n}\n}\n}\n}\nmore_code_that_should_be_dropped"
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "x = 1\n}\n}\n}"


def test_clean_code_does_not_truncate_repeated_long_lines():
    # The repeat guard only applies to short (<3 char) noise lines, so
    # legitimately repeated longer lines are left alone.
    code = "abc\nabc\nabc\nabc"
    assert coding_agent.CodeGenerationLLM._clean_code(code) == code


def test_clean_code_leaves_stray_fence_when_followed_by_explanation():
    # Documents current behavior: the trailing-fence strip only applies
    # when the "```" is at the very end of the whole string. If prose
    # follows the closing fence, that "```" survives as an ordinary line
    # (it isn't one of the truncation trigger prefixes) right up until the
    # explanation line itself triggers the cutoff.
    code = "```python\ndef foo():\n    return 1\n```\nNote: returns 1."
    assert coding_agent.CodeGenerationLLM._clean_code(code) == "def foo():\n    return 1\n```"


# ---- CodingAgent.should_continue ----

@pytest.mark.parametrize(
    "status,expected",
    [
        ("executing", "execute"),
        ("fixing", "fix"),
        ("planning", "end"),
        ("completed", "end"),
        ("failed", "end"),
    ],
)
def test_should_continue(status, expected):
    agent = coding_agent.CodingAgent(llm=None, executor=None)
    assert agent.should_continue(make_state(status=status)) == expected


# ---- CodingAgent node functions ----

def test_generate_code_node_calls_llm_and_updates_state():
    llm = MagicMock()
    llm.generate_code_sync.return_value = "print('hi')"
    agent = coding_agent.CodingAgent(llm=llm, executor=MagicMock())
    state = make_state()

    result = asyncio.run(agent.generate_code_node(state))

    llm.generate_code_sync.assert_called_once_with(state["user_request"])
    assert result["generated_code"] == "print('hi')"
    assert result["status"] == "executing"


def test_execute_code_node_success_marks_completed():
    executor = MagicMock()
    executor.execute = AsyncMock(return_value=("hi\n", True))
    agent = coding_agent.CodingAgent(llm=MagicMock(), executor=executor)
    state = make_state(generated_code="print('hi')", status="executing")

    result = asyncio.run(agent.execute_code_node(state))

    executor.execute.assert_awaited_once_with("print('hi')")
    assert result["status"] == "completed"
    assert result["execution_result"] == "hi\n"
    assert result["error_message"] is None


def test_execute_code_node_failure_routes_to_fixing():
    executor = MagicMock()
    executor.execute = AsyncMock(return_value=("Traceback...", False))
    agent = coding_agent.CodingAgent(llm=MagicMock(), executor=executor)
    state = make_state(generated_code="bad code", status="executing", iteration_count=1)

    result = asyncio.run(agent.execute_code_node(state))

    assert result["status"] == "fixing"
    assert result["error_message"] == "Traceback..."
    assert result["iteration_count"] == 2


def test_fix_code_node_calls_llm_when_under_max_iterations():
    llm = MagicMock()
    llm.fix_code_sync.return_value = "fixed code"
    agent = coding_agent.CodingAgent(llm=llm, executor=MagicMock())
    state = make_state(iteration_count=1, max_iterations=3, generated_code="bad code", error_message="NameError")

    result = asyncio.run(agent.fix_code_node(state))

    llm.fix_code_sync.assert_called_once_with(state["user_request"], "bad code", "NameError")
    assert result["generated_code"] == "fixed code"
    assert result["status"] == "executing"


def test_fix_code_node_gives_up_at_max_iterations():
    llm = MagicMock()
    agent = coding_agent.CodingAgent(llm=llm, executor=MagicMock())
    state = make_state(iteration_count=3, max_iterations=3)

    result = asyncio.run(agent.fix_code_node(state))

    llm.fix_code_sync.assert_not_called()
    assert result["status"] == "failed"
    assert "Max iterations reached (3)" in result["execution_result"]


# ---- LocalCodeExecutor.execute ----

def _fake_process(stdout=b"", stderr=b"", returncode=0):
    proc = MagicMock()
    proc.communicate = AsyncMock(return_value=(stdout, stderr))
    proc.wait = AsyncMock(return_value=None)
    proc.kill = MagicMock()
    proc.returncode = returncode
    return proc


def test_execute_success_returns_stdout():
    proc = _fake_process(stdout=b"hello\n", returncode=0)
    with patch("coding_agent.asyncio.create_subprocess_exec", new=AsyncMock(return_value=proc)):
        output, success = asyncio.run(coding_agent.LocalCodeExecutor.execute("print('hello')"))

    assert success is True
    assert output == "hello\n"


def test_execute_failure_returns_stderr():
    proc = _fake_process(stderr=b"Traceback...\n", returncode=1)
    with patch("coding_agent.asyncio.create_subprocess_exec", new=AsyncMock(return_value=proc)):
        output, success = asyncio.run(coding_agent.LocalCodeExecutor.execute("raise ValueError()"))

    assert success is False
    assert output == "Traceback...\n"


def test_execute_timeout_kills_process(monkeypatch):
    proc = _fake_process()

    async def raise_timeout(awaitable, timeout=None):
        awaitable.close()  # avoid a "coroutine was never awaited" warning
        raise asyncio.TimeoutError()

    with patch("coding_agent.asyncio.create_subprocess_exec", new=AsyncMock(return_value=proc)), \
         patch("coding_agent.asyncio.wait_for", new=raise_timeout):
        output, success = asyncio.run(coding_agent.LocalCodeExecutor.execute("while True: pass"))

    assert success is False
    assert output == "Execution timeout (60s)"
    proc.kill.assert_called_once()
    proc.wait.assert_awaited_once()


def test_execute_generic_exception_is_reported():
    with patch("coding_agent.asyncio.create_subprocess_exec", new=AsyncMock(side_effect=OSError("boom"))):
        output, success = asyncio.run(coding_agent.LocalCodeExecutor.execute("print(1)"))

    assert success is False
    assert output == "Execution error: boom"


def test_execute_passes_only_allowlisted_env_vars(monkeypatch):
    monkeypatch.setenv("PATH", "/usr/bin")
    monkeypatch.setenv("LANG", "en_US.UTF-8")
    monkeypatch.setenv("HF_TOKEN", "super-secret")
    monkeypatch.delenv("SUBPROCESS_ENV_PASSTHROUGH", raising=False)

    proc = _fake_process(stdout=b"ok", returncode=0)
    create_mock = AsyncMock(return_value=proc)
    with patch("coding_agent.asyncio.create_subprocess_exec", new=create_mock):
        asyncio.run(coding_agent.LocalCodeExecutor.execute("print('ok')"))

    passed_env = create_mock.call_args.kwargs["env"]
    assert passed_env.get("PATH") == "/usr/bin"
    assert passed_env.get("LANG") == "en_US.UTF-8"
    assert "HF_TOKEN" not in passed_env


def test_execute_respects_extra_passthrough_env_var(monkeypatch):
    monkeypatch.setenv("SUBPROCESS_ENV_PASSTHROUGH", "MY_EXTRA_VAR")
    monkeypatch.setenv("MY_EXTRA_VAR", "value123")

    proc = _fake_process(stdout=b"ok", returncode=0)
    create_mock = AsyncMock(return_value=proc)
    with patch("coding_agent.asyncio.create_subprocess_exec", new=create_mock):
        asyncio.run(coding_agent.LocalCodeExecutor.execute("print('ok')"))

    passed_env = create_mock.call_args.kwargs["env"]
    assert passed_env.get("MY_EXTRA_VAR") == "value123"
