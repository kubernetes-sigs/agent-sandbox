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

"""
Unit tests for the kata-aks per-owner agent.

agent.py runs as a FastAPI server backed by an OpenAI-compatible endpoint;
these tests cover its required-settings validation, per-owner system-prompt
construction, and the bounded in-memory chat history, with the OpenAI client
mocked. No kind/AKS cluster, Kata node pool, or container build is needed.

agent.py raises at import time if OPENAI_BASE_URL/OPENAI_API_KEY/LLM_MODEL
are unset, so each test loads its own copy by file path with those env vars
controlled first, mirroring
examples/hermes-agents-as-a-service/gateway/test_gateway.py.
"""

import importlib.util
import os
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from fastapi.testclient import TestClient

AGENT_PATH = os.path.join(os.path.dirname(__file__), "agent.py")

REQUIRED_ENV = {
    "OPENAI_BASE_URL": "http://example.invalid/v1",
    "OPENAI_API_KEY": "test-key",
    "LLM_MODEL": "test-model",
}


def _load_agent(monkeypatch, env):
    for var in ("OPENAI_BASE_URL", "OPENAI_API_KEY", "LLM_MODEL"):
        monkeypatch.delenv(var, raising=False)
    for key, value in env.items():
        monkeypatch.setenv(key, value)

    spec = importlib.util.spec_from_file_location("kata_aks_agent", AGENT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def make_completion(content):
    """Builds a MagicMock matching the shape of an OpenAI ChatCompletion."""
    completion = MagicMock()
    completion.choices = [MagicMock(message=MagicMock(content=content))]
    return completion


@pytest.fixture
def agent(monkeypatch):
    """A freshly loaded agent module (own _history state) with its OpenAI
    client replaced by a MagicMock, plus a FastAPI test client."""
    module = _load_agent(monkeypatch, REQUIRED_ENV)
    module._client = MagicMock()
    return SimpleNamespace(module=module, client=TestClient(module.app), openai=module._client)


# --- required settings -------------------------------------------------------

def test_raises_when_all_settings_missing(monkeypatch):
    with pytest.raises(RuntimeError) as exc_info:
        _load_agent(monkeypatch, {})
    for name in ("OPENAI_BASE_URL", "OPENAI_API_KEY", "LLM_MODEL"):
        assert name in str(exc_info.value)


def test_raises_naming_only_the_missing_setting(monkeypatch):
    env = dict(REQUIRED_ENV)
    del env["LLM_MODEL"]

    with pytest.raises(RuntimeError) as exc_info:
        _load_agent(monkeypatch, env)

    message = str(exc_info.value)
    assert "LLM_MODEL" in message
    assert "OPENAI_BASE_URL" not in message
    assert "OPENAI_API_KEY" not in message


def test_loads_successfully_with_all_settings(monkeypatch):
    module = _load_agent(monkeypatch, REQUIRED_ENV)
    assert module.app is not None


# --- /healthz ------------------------------------------------------------

def test_healthz(agent):
    response = agent.client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"ok": True}


# --- /chat -----------------------------------------------------------------

def test_chat_names_the_owner_in_the_system_prompt(agent):
    agent.openai.chat.completions.create.return_value = make_completion("hi there")

    response = agent.client.post("/chat", json={"prompt": "hello"}, headers={"X-Owner": "alice"})

    assert response.status_code == 200
    assert response.json() == {"owner": "alice", "reply": "hi there", "history_turns": 1}

    messages = agent.openai.chat.completions.create.call_args.kwargs["messages"]
    assert messages[0]["role"] == "system"
    assert "alice's personal AI agent" in messages[0]["content"]
    assert "I am alice's agent." in messages[0]["content"]
    assert messages[-1] == {"role": "user", "content": "hello"}


def test_chat_defaults_owner_to_anonymous_when_header_missing(agent):
    agent.openai.chat.completions.create.return_value = make_completion("hi")

    response = agent.client.post("/chat", json={"prompt": "hello"})

    assert response.status_code == 200
    assert response.json()["owner"] == "anonymous"


def test_chat_includes_prior_turns_and_reports_turn_count(agent):
    agent.openai.chat.completions.create.side_effect = [
        make_completion("first reply"),
        make_completion("second reply"),
    ]

    first = agent.client.post("/chat", json={"prompt": "one"}, headers={"X-Owner": "bob"})
    assert first.json()["history_turns"] == 1

    second = agent.client.post("/chat", json={"prompt": "two"}, headers={"X-Owner": "bob"})
    assert second.json()["history_turns"] == 2

    second_messages = agent.openai.chat.completions.create.call_args.kwargs["messages"]
    assert second_messages[1] == {"role": "user", "content": "one"}
    assert second_messages[2] == {"role": "assistant", "content": "first reply"}
    assert second_messages[3] == {"role": "user", "content": "two"}


def test_chat_history_is_bounded_per_owner(agent):
    # _HISTORY_TURNS is read by the defaultdict factory the first time this
    # owner's history is touched, so patching it here caps "alice"'s deque
    # at 2 turns (4 messages) without needing 41 requests to prove eviction.
    agent.module._HISTORY_TURNS = 2
    agent.openai.chat.completions.create.side_effect = [make_completion(f"reply-{i}") for i in range(5)]

    for i in range(5):
        response = agent.client.post(
            "/chat", json={"prompt": f"prompt-{i}"}, headers={"X-Owner": "alice"})
        assert response.status_code == 200

    history = list(agent.module._history["alice"])
    assert len(history) == 4  # maxlen = 2 * _HISTORY_TURNS -- oldest turns evicted
    assert history[-2] == {"role": "user", "content": "prompt-4"}
    assert history[-1] == {"role": "assistant", "content": "reply-4"}


def test_chat_history_is_isolated_per_owner(agent):
    agent.openai.chat.completions.create.side_effect = [
        make_completion("a-reply"), make_completion("b-reply")]

    agent.client.post("/chat", json={"prompt": "hi"}, headers={"X-Owner": "alice"})
    agent.client.post("/chat", json={"prompt": "hi"}, headers={"X-Owner": "bob"})

    assert list(agent.module._history["alice"]) == [
        {"role": "user", "content": "hi"}, {"role": "assistant", "content": "a-reply"}]
    assert list(agent.module._history["bob"]) == [
        {"role": "user", "content": "hi"}, {"role": "assistant", "content": "b-reply"}]


# --- /reset ------------------------------------------------------------------

def test_reset_clears_only_that_owners_history(agent):
    agent.module._history["alice"].extend([{"role": "user", "content": "x"}])
    agent.module._history["bob"].extend([{"role": "user", "content": "y"}])

    response = agent.client.post("/reset", headers={"X-Owner": "alice"})

    assert response.status_code == 200
    assert response.json() == {"owner": "alice", "reset": True}
    assert "alice" not in agent.module._history
    assert list(agent.module._history["bob"]) == [{"role": "user", "content": "y"}]


def test_reset_defaults_owner_to_anonymous(agent):
    response = agent.client.post("/reset")
    assert response.json() == {"owner": "anonymous", "reset": True}
