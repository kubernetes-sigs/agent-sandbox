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

"""Unit tests for the RLlib-to-SandboxEnv adaptation layer."""

import gymnasium as gym
from gymnasium import spaces
import numpy as np
import pytest
import file_task_env

from file_task_env import (
    CREATE_DIRECTORY,
    DiscreteFileTaskWrapper,
    FileTaskReward,
    FileTaskTermination,
    REMOVE_OUTPUT,
    WRITE_FILE,
    command_for_action,
    parse_file_task_state,
)


class FakeSandboxEnv(gym.Env):
    def __init__(self, process_channel=None):
        from types import SimpleNamespace
        self.action_space = spaces.Text(max_length=2048)
        self.observation_space = spaces.Text(max_length=4096)
        self.commands = []
        self.closed = False
        self.step_count = 0
        self.state = [0, 0, 0]
        self.resets = 0
        self._sandbox = SimpleNamespace(connector=SimpleNamespace(
            connect=lambda: None, grpc_channel=lambda: process_channel,
        ))

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        self.step_count = 0
        self.state = [0, 0, 0]
        self.resets += 1
        suffix = "test" if self.resets == 1 else str(self.resets)
        return "Sandbox ready.", {
            "claim_name": f"sandbox-claim-{suffix}",
            "sandbox_id": f"sandbox-{suffix}",
        }

    def step(self, command):
        self.commands.append(command)
        self.step_count += 1
        if "mkdir -p /tmp/agent-sandbox-rllib/output" in command:
            self.state[0] = 1
        elif "rm -rf /tmp/agent-sandbox-rllib" in command:
            self.state = [0, 0, 0]
        elif "> /tmp/agent-sandbox-rllib/output/answer.txt" in command and self.state[0]:
            self.state[1:] = [1, 1]

        observation = "FILE_TASK_STATE=" + ",".join(map(str, self.state)) + "\n"
        terminated = bool(self.state[2])
        reward = 1.0 if terminated else -0.05
        return (
            observation,
            reward,
            terminated,
            self.step_count >= 4 and not terminated,
            {"step": self.step_count, "env_error": False, "exit_code": 0},
        )

    def close(self):
        self.closed = True


class FakeClient:
    def __init__(self, process_channel=None):
        self.delete_all_calls = 0
        self.process_channel = process_channel

    def delete_all(self):
        self.delete_all_calls += 1

    def get_sandbox(self, *args, **kwargs):
        from types import SimpleNamespace
        return SimpleNamespace(connector=SimpleNamespace(
            connect=lambda: None, grpc_channel=lambda: self.process_channel,
        ))


@pytest.fixture
def process_channel():
    from concurrent.futures import ThreadPoolExecutor
    import grpc

    server = grpc.server(ThreadPoolExecutor(max_workers=1))
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    channel = grpc.insecure_channel(f"127.0.0.1:{port}")
    try:
        yield channel
    finally:
        server.stop(0).wait()
        channel.close()


def test_parse_file_task_state():
    state = parse_file_task_state("FILE_TASK_STATE=1,0,1\n")

    assert state.directory_exists
    assert not state.file_exists
    assert state.content_is_correct


def test_parse_file_task_state_rejects_missing_marker():
    with pytest.raises(ValueError, match="missing FILE_TASK_STATE"):
        parse_file_task_state("command failed")


def test_command_for_action_rejects_unknown_action():
    with pytest.raises(ValueError, match="Unsupported file-task action"):
        command_for_action(99)


@pytest.mark.parametrize("action", [1.5, "1"])
def test_command_for_action_rejects_non_integer_action(action):
    with pytest.raises(ValueError, match="Unsupported file-task action"):
        command_for_action(action)


def test_command_for_action_accepts_numpy_integer():
    command = command_for_action(np.int64(CREATE_DIRECTORY))

    assert "mkdir -p /tmp/agent-sandbox-rllib/output" in command


def test_wrapper_maps_actions_and_observations():
    base_env = FakeSandboxEnv()
    client = FakeClient()
    env = DiscreteFileTaskWrapper(
        base_env,
        client=client,
        max_episode_steps=4,
    )

    observation, info = env.reset()
    assert info["claim_name"] == "sandbox-claim-test"
    np.testing.assert_array_equal(observation, [0.0, 0.0, 0.0, 1.0])

    observation, reward, terminated, truncated, info = env.step(
        CREATE_DIRECTORY
    )
    np.testing.assert_array_equal(observation, [1.0, 0.0, 0.0, 0.75])
    assert reward == -0.05
    assert not terminated
    assert not truncated
    assert info["action_name"] == "create-directory"

    observation, reward, terminated, truncated, info = env.step(WRITE_FILE)
    np.testing.assert_array_equal(observation, [1.0, 1.0, 1.0, 0.5])
    assert reward == 1.0
    assert terminated
    assert not truncated
    assert info["success"]

    env.close()
    assert base_env.closed
    assert client.delete_all_calls == 1


def test_remove_output_maps_to_fixed_command():
    command = command_for_action(REMOVE_OUTPUT)

    assert command.startswith("sh -c ")
    assert "rm -rf /tmp/agent-sandbox-rllib" in command
    assert "FILE_TASK_STATE" in command


def test_environment_error_truncates_episode_and_preserves_state():
    base_env = FakeSandboxEnv()
    env = DiscreteFileTaskWrapper(base_env, max_episode_steps=4)
    env.reset()

    def failed_step(command):
        del command
        return "connection lost", -1.0, False, False, {
            "step": 1,
            "env_error": True,
        }

    base_env.step = failed_step
    observation, reward, terminated, truncated, info = env.step(
        CREATE_DIRECTORY
    )

    np.testing.assert_array_equal(observation, [0.0, 0.0, 0.0, 0.75])
    assert reward == -1.0
    assert not terminated
    assert truncated
    assert info["state_parse_error"]


def test_state_parse_error_truncates_episode():
    base_env = FakeSandboxEnv()
    env = DiscreteFileTaskWrapper(base_env, max_episode_steps=4)
    env.reset()

    def malformed_step(command):
        del command
        return "unexpected output", -1.0, False, False, {
            "step": 1,
            "env_error": False,
        }

    base_env.step = malformed_step
    observation, reward, terminated, truncated, info = env.step(
        CREATE_DIRECTORY
    )

    np.testing.assert_array_equal(observation, [0.0, 0.0, 0.0, 0.75])
    assert reward == -1.0
    assert not terminated
    assert truncated
    assert info["state_parse_error"]


def test_close_cleans_up_client_when_environment_close_fails():
    base_env = FakeSandboxEnv()
    client = FakeClient()
    env = DiscreteFileTaskWrapper(base_env, client=client)

    def failed_close():
        raise RuntimeError("close failed")

    base_env.close = failed_close
    with pytest.raises(RuntimeError, match="close failed"):
        env.close()

    assert client.delete_all_calls == 1


def test_reward_and_termination_follow_encoded_state():
    reward = FileTaskReward()
    termination = FileTaskTermination()
    info = {"env_error": False}

    assert reward("command", "FILE_TASK_STATE=1,0,0\n", info, "task") == -0.05
    assert not termination("FILE_TASK_STATE=1,0,0\n", info, "task")
    assert reward("command", "FILE_TASK_STATE=1,1,1\n", info, "task") == 1.0
    assert termination("FILE_TASK_STATE=1,1,1\n", info, "task")


def test_evidence_retains_every_episode_and_returns_a_copy():
    env = DiscreteFileTaskWrapper(FakeSandboxEnv(), namespace="training")
    env.reset()
    env.step(CREATE_DIRECTORY)
    env.reset()
    env.step(WRITE_FILE)

    evidence = env.get_claim_evidence()
    assert evidence["claims"] == [
        {"namespace": "training", "claim_name": "sandbox-claim-test", "sandbox_id": "sandbox-test"},
        {"namespace": "training", "claim_name": "sandbox-claim-2", "sandbox_id": "sandbox-2"},
    ]
    assert evidence["steps"] == evidence["successful_steps"] == 2
    assert evidence["errors"] == 0
    assert evidence["env_id"] != DiscreteFileTaskWrapper(FakeSandboxEnv()).get_claim_evidence()["env_id"]
    evidence["claims"][0]["claim_name"] = "mutated"
    assert env.get_claim_evidence()["claims"][0]["claim_name"] == "sandbox-claim-test"


@pytest.mark.parametrize("exit_code,env_error,output", [
    (1, False, "FILE_TASK_STATE=1,1,1\n"),
    (0, True, "FILE_TASK_STATE=1,1,1\n"),
    (0, False, "malformed"),
])
def test_failed_execution_is_not_successful_evidence(exit_code, env_error, output):
    base_env = FakeSandboxEnv()
    env = DiscreteFileTaskWrapper(base_env)
    env.reset()
    base_env.step = lambda command: (output, 1.0, True, False, {
        "step": 1, "exit_code": exit_code, "env_error": env_error,
    })
    _, _, terminated, truncated, info = env.step(WRITE_FILE)
    assert truncated
    assert not terminated
    assert not info["success"]
    assert env.get_claim_evidence()["errors"] == 1
    assert env.get_claim_evidence()["successful_steps"] == 0


@pytest.mark.parametrize("mode,config_type", [
    ("tunnel", "SandboxLocalTunnelConnectionConfig"),
    ("in-cluster", "SandboxInClusterConnectionConfig"),
    ("sandboxd-in-cluster", "SandboxdInClusterConnectionConfig"),
])
def test_environment_constructs_an_independent_client_with_selected_transport(monkeypatch, process_channel, mode, config_type):
    configs = []
    def make_client(*, connection_config, cleanup):
        assert cleanup
        configs.append(connection_config)
        return FakeClient(process_channel)
    monkeypatch.setattr(file_task_env, "SandboxClient", make_client)
    monkeypatch.setattr(file_task_env, "SandboxEnv", lambda **kwargs: FakeSandboxEnv(process_channel))
    first = file_task_env.SandboxFileTaskEnv({"connection_mode": mode, "namespace": "training"})
    second = file_task_env.SandboxFileTaskEnv({"connection_mode": mode})
    assert type(configs[0]).__name__ == config_type
    assert configs[0] is not configs[1]
    if mode == "sandboxd-in-cluster":
        assert configs[0].mode == "service-dns"
    first.reset()
    assert first.get_claim_evidence()["claims"][0]["namespace"] == "training"
    first.close()
    second.close()


def test_environment_rejects_unknown_transport_before_creating_client():
    with pytest.raises(ValueError, match="Unsupported connection mode"):
        file_task_env.SandboxFileTaskEnv({"connection_mode": "unknown"})


@pytest.mark.parametrize("endpoint_state", ["ready", "unreachable", "no-service"])
def test_sandboxd_readiness_reuses_the_episode_connection_until_reset_or_close(monkeypatch, endpoint_state):
    from concurrent.futures import ThreadPoolExecutor
    from types import SimpleNamespace
    import grpc
    from k8s_agent_sandbox.sandbox import Sandbox
    from k8s_agent_sandbox.commands._process_stubs import process_pb2, process_pb2_grpc
    from k8s_agent_sandbox.models import SandboxdInClusterConnectionConfig
    from k8s_agent_sandbox.exceptions import SandboxServiceUnavailableError

    class ProcessService(process_pb2_grpc.ProcessServiceServicer):
        def Execute(self, request, context):
            return process_pb2.ExecuteResponse(
                stdout=b"FILE_TASK_STATE=1,0,0\n", exit_code=0,
            )

    server = grpc.server(ThreadPoolExecutor(max_workers=1))
    process_pb2_grpc.add_ProcessServiceServicer_to_server(ProcessService(), server)
    port = server.add_insecure_port("127.0.0.1:0")
    if endpoint_state == "ready":
        server.start()
    handles = []
    deleted_claims = []
    helper = SimpleNamespace(
        get_sandbox=lambda *args: {
            "status": {"serviceFQDN": None if endpoint_state == "no-service" else "127.0.0.1"},
        },
        delete_sandbox_claim=lambda name, namespace: deleted_claims.append((namespace, name)),
    )

    class EpisodeClient:
        def create_sandbox(self, *, warmpool, namespace):
            sandbox = Sandbox(
                claim_name=f"claim-{len(handles)}", sandbox_id=f"sandbox-{len(handles)}",
                namespace=namespace, k8s_helper=helper,
                connection_config=SandboxdInClusterConnectionConfig(
                    mode="service-dns", grpc_port=port,
                ),
            )
            handles.append(sandbox)
            return sandbox

        def get_sandbox(self, *args, **kwargs):
            raise AssertionError("readiness must reuse the episode's existing Sandbox")

        def delete_sandbox(self, name, *, namespace):
            for sandbox in handles:
                if sandbox.claim_name == name and sandbox.namespace == namespace:
                    sandbox.terminate()

        def delete_all(self):
            for sandbox in handles:
                sandbox.terminate()

    monkeypatch.setattr(file_task_env, "SandboxClient", lambda **kwargs: EpisodeClient())
    # Keep the real Gym integration, SDK Sandbox, connector and gRPC transport.
    env = file_task_env.SandboxFileTaskEnv({
        "connection_mode": "sandboxd-in-cluster", "namespace": "training",
        "step_timeout_seconds": 1,
    })
    try:
        if endpoint_state != "ready":
            error = grpc.FutureTimeoutError if endpoint_state == "unreachable" else SandboxServiceUnavailableError
            with pytest.raises(error):
                env.reset()
            evidence = env.get_claim_evidence()
            assert evidence["claims"][0]["claim_name"] == "claim-0"
            assert evidence["steps"] == 0
            channel = handles[0].connector.grpc_channel() if endpoint_state == "unreachable" else None
            env.close()
            assert deleted_claims == [("training", "claim-0")]
            assert not handles[0].is_active
            if channel is not None:
                with pytest.raises(ValueError, match="closed"):
                    channel.unary_unary("/unused")(b"", timeout=1)
            return
        env.reset()
        first_channel = handles[0].connector.grpc_channel()
        _, _, _, truncated, info = env.step(CREATE_DIRECTORY)
        assert not truncated and not info["env_error"] and info["exit_code"] == 0

        env.reset()
        assert deleted_claims == [("training", "claim-0")]
        assert not handles[0].is_active
        with pytest.raises(ValueError, match="closed"):
            first_channel.unary_unary("/unused")(b"", timeout=1)
        second_channel = handles[1].connector.grpc_channel()
        _, _, _, truncated, info = env.step(CREATE_DIRECTORY)
        assert not truncated and not info["env_error"] and info["exit_code"] == 0

        env.close()
        assert deleted_claims == [("training", "claim-0"), ("training", "claim-1")]
        assert not handles[1].is_active
        with pytest.raises(ValueError, match="closed"):
            second_channel.unary_unary("/unused")(b"", timeout=1)
    finally:
        env.close()
        server.stop(0).wait()


def test_sandboxd_reset_waits_for_the_process_channel_to_be_ready(monkeypatch):
    from concurrent.futures import ThreadPoolExecutor
    from threading import Event, Timer
    import socket
    import grpc

    server = grpc.server(ThreadPoolExecutor(max_workers=1))
    reserved = socket.socket()
    reserved.bind(("127.0.0.1", 0))
    port = reserved.getsockname()[1]
    channel = grpc.insecure_channel(f"127.0.0.1:{port}")
    started = Event()
    def start_server():
        reserved.close()
        started.set()
        server.add_insecure_port(f"127.0.0.1:{port}")
        server.start()
    client = FakeClient()
    monkeypatch.setattr(file_task_env, "SandboxClient", lambda **kwargs: client)
    monkeypatch.setattr(file_task_env, "SandboxEnv", lambda **kwargs: FakeSandboxEnv(channel))
    env = file_task_env.SandboxFileTaskEnv({"connection_mode": "sandboxd-in-cluster"})
    timer = Timer(0.1, start_server)
    timer.start()
    try:
        _, info = env.reset()
        assert started.is_set(), "reset returned before the process service was available"
        # Probe the live server without starting another connectivity watcher
        # that could race the fixture's channel closure.
        with pytest.raises(grpc.RpcError) as error:
            channel.unary_unary("/readiness-probe")(b"", timeout=5)
        assert error.value.code() == grpc.StatusCode.UNIMPLEMENTED
        assert info["claim_name"] == "sandbox-claim-test"
        assert env.get_claim_evidence()["steps"] == 0
    finally:
        timer.join()
        env.close()
        server.stop(0).wait()
        channel.close()


def test_sandboxd_reset_times_out_and_keeps_the_claim_available_for_cleanup(monkeypatch):
    import socket
    import grpc

    reserved = socket.socket()
    reserved.bind(("127.0.0.1", 0))
    channel = grpc.insecure_channel(f"127.0.0.1:{reserved.getsockname()[1]}")
    client = FakeClient()
    base_env = FakeSandboxEnv(channel)
    monkeypatch.setattr(file_task_env, "SandboxClient", lambda **kwargs: client)
    monkeypatch.setattr(file_task_env, "SandboxEnv", lambda **kwargs: base_env)
    env = file_task_env.SandboxFileTaskEnv({
        "connection_mode": "sandboxd-in-cluster", "step_timeout_seconds": 1,
    })
    try:
        with pytest.raises(grpc.FutureTimeoutError):
            env.reset()
        evidence = env.get_claim_evidence()
        assert evidence["claims"][0]["claim_name"] == "sandbox-claim-test"
        assert evidence["steps"] == 0
    finally:
        env.close()
        channel.close()
        reserved.close()
    assert base_env.closed
    assert client.delete_all_calls == 1


def test_preflight_runs_real_wrapper_commands_and_rest_on_the_same_claim(monkeypatch, process_channel):
    from types import SimpleNamespace
    from verify_sandbox_task import verify_file_task
    stored = {}
    lookups = []
    client = FakeClient(process_channel)
    connector = client.get_sandbox("sandbox-claim-test", namespace="training").connector
    def get_sandbox(name, namespace):
        lookups.append((namespace, name))
        return SimpleNamespace(connector=connector, files=SimpleNamespace(
            write=lambda path, content: stored.update({path: content.encode("utf-8")}),
            read=lambda path: stored[path],
        ))
    client.get_sandbox = get_sandbox
    base_env = FakeSandboxEnv(process_channel)
    monkeypatch.setattr(file_task_env, "SandboxClient", lambda **kwargs: client)
    monkeypatch.setattr(file_task_env, "SandboxEnv", lambda **kwargs: base_env)
    evidence = []
    verify_file_task({"namespace": "training", "connection_mode": "sandboxd-in-cluster"},
                     evidence=evidence, verify_rest=True)
    assert evidence[0]["successful_steps"] == 2
    assert evidence[0]["errors"] == 0
    assert set(lookups) == {("training", "sandbox-claim-test")}
    assert stored == {"rllib-preflight.txt": b"sandbox-ready"}
    assert base_env.closed
    assert client.delete_all_calls == 1


def test_finalization_retains_evidence_without_masking_the_task_error():
    from verify_sandbox_task import close_environment
    base_env = FakeSandboxEnv()
    env = DiscreteFileTaskWrapper(base_env)
    env.reset()
    def failed_close():
        raise RuntimeError("secondary close error")
    base_env.close = failed_close
    evidence = []
    with pytest.raises(ValueError, match="primary task error") as error:
        try:
            raise ValueError("primary task error")
        finally:
            close_environment(env, evidence=evidence)
    assert evidence[0]["claims"][0]["claim_name"] == "sandbox-claim-test"
    assert isinstance(error.value.__cause__, RuntimeError)
