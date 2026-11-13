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

"""Unit tests for run_ci_job.py with a mocked Sandbox SDK (no cluster)."""

from types import SimpleNamespace

import pytest

import run_ci_job


class FakeSandbox:
    """Records commands and replays canned ExecutionResult-like objects."""

    def __init__(self, results):
        self.results = list(results)
        self.executed = []
        self.terminated = False
        self.commands = SimpleNamespace(run=self._run)

    def _run(self, command, command_timeout=None):
        self.executed.append((command, command_timeout))
        return self.results.pop(0)

    def terminate(self):
        self.terminated = True


def result(exit_code=0, stdout="", stderr="", timed_out=False):
    return SimpleNamespace(
        exit_code=exit_code, stdout=stdout, stderr=stderr, timed_out=timed_out
    )


class FakeClient:
    def __init__(self, sandbox):
        self.sandbox = sandbox
        self.create_kwargs = None

    def create_sandbox(self, **kwargs):
        self.create_kwargs = kwargs
        return self.sandbox


def test_run_commands_all_succeed():
    sandbox = FakeSandbox([result(stdout="ok1"), result(stdout="ok2")])
    code = run_ci_job.run_commands(sandbox, ["make build", "make test"], 60)
    assert code == 0
    assert [c for c, _ in sandbox.executed] == ["make build", "make test"]
    # The per-command timeout must reach the SDK so long builds get killed.
    assert all(t == 60 for _, t in sandbox.executed)


def test_run_commands_stops_at_first_failure():
    sandbox = FakeSandbox([result(exit_code=2, stderr="boom"), result()])
    code = run_ci_job.run_commands(sandbox, ["false-cmd", "never-run"], 60)
    assert code == 2
    assert [c for c, _ in sandbox.executed] == ["false-cmd"]


def test_run_commands_timeout_maps_to_124():
    sandbox = FakeSandbox([result(exit_code=124, timed_out=True)])
    code = run_ci_job.run_commands(sandbox, ["sleep 9999"], 60)
    assert code == 124


def test_main_terminates_sandbox_even_on_driver_error():
    class ExplodingSandbox(FakeSandbox):
        def terminate(self):
            super().terminate()

    sandbox = ExplodingSandbox([])
    sandbox.commands = SimpleNamespace(run=lambda *a, **k: (_ for _ in ()).throw(
        RuntimeError("connection lost")
    ))
    client = FakeClient(sandbox)
    with pytest.raises(RuntimeError):
        run_ci_job.main(["--", "make test"], client_factory=lambda: client)
    assert sandbox.terminated


def test_main_passes_claim_name_and_ttl_to_sdk():
    sandbox = FakeSandbox([result()])
    client = FakeClient(sandbox)
    code = run_ci_job.main(
        ["--namespace", "ci", "--claim-name", "Run_42/x", "--ttl", "600",
         "--", "echo hi"],
        client_factory=lambda: client,
    )
    assert code == 0
    assert client.create_kwargs["namespace"] == "ci"
    assert client.create_kwargs["claim_name"] == "ci-run-42-x"
    assert client.create_kwargs["shutdown_after_seconds"] == 600
    assert sandbox.terminated


@pytest.mark.parametrize(
    "raw,expected",
    [
        ("Run_42", "run-42"),
        ("UPPER Case!", "upper-case"),
        ("--leading-trailing--", "leading-trailing"),
        ("", ""),
        ("***", ""),
    ],
)
def test_sanitize_claim_name(raw, expected):
    assert run_ci_job.sanitize_claim_name(raw) == expected


def test_claim_name_falls_back_when_run_id_unusable():
    assert run_ci_job.build_claim_name("***") == "ci-job"
    assert run_ci_job.build_claim_name("123") == "ci-123"
