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

"""Lightweight acceptance tests; no Ray, Torch or Kubernetes cluster required."""

from copy import deepcopy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Event, Thread

import pytest
from kubernetes import client
from kubernetes.client.exceptions import ApiException

from verify_sandbox_task import (
    claim_lookup, validate_runner_evidence, training_counters, wait_for_claim_deletion,
    prepare_run_directory, validate_checkpoint,
)


def runner_records():
    return [{
        "worker_index": index,
        "actor_id": f"actor-{index}",
        "node_id": "shared-node",
        "envs": [{
            "env_id": f"env-{index}", "steps": 2, "successful_steps": 2, "errors": 0,
            "claims": [{"namespace": "training", "claim_name": f"claim-{index}", "sandbox_id": f"sandbox-{index}"}],
        }],
    } for index in (1, 2)]


def test_remote_evidence_accepts_shared_node_but_independent_executing_actors():
    records = runner_records()
    assert validate_runner_evidence(records, expected_runners=2) == [
        records[0]["envs"][0]["claims"][0], records[1]["envs"][0]["claims"][0],
    ]


@pytest.mark.parametrize("invalid", ["missing", "actor", "claim", "sandbox", "zero", "errors", "unknown", "local", "empty-env"])
def test_remote_evidence_rejects_incomplete_or_nonindependent_work(invalid):
    records = deepcopy(runner_records())
    if invalid == "missing":
        records.pop()
    elif invalid == "actor":
        records[1]["actor_id"] = records[0]["actor_id"]
    elif invalid in ("claim", "sandbox"):
        key = "claim_name" if invalid == "claim" else "sandbox_id"
        records[1]["envs"][0]["claims"][0][key] = records[0]["envs"][0]["claims"][0][key]
    elif invalid == "zero":
        records[1]["envs"][0]["successful_steps"] = 0
    elif invalid == "errors":
        records[1]["envs"][0]["errors"] = 1
    elif invalid == "unknown":
        records[1]["actor_id"] = None
    elif invalid == "local":
        records[1]["worker_index"] = 0
    elif invalid == "empty-env":
        records[1]["envs"] = []
    with pytest.raises(RuntimeError):
        validate_runner_evidence(records, expected_runners=2)


@pytest.mark.parametrize("sampled,trained", [(64, 64), (128, 768)])
def test_counters_require_real_sampling_and_learning(sampled, trained):
    result = {"env_runners": {"num_env_steps_sampled_lifetime": sampled},
              "learners": {"__all_modules__": {"num_env_steps_trained_lifetime": trained}}}
    assert training_counters(result) == {"sampled": sampled, "trained": trained}


@pytest.mark.parametrize("result", [{}, {"training_iteration": 5},
    {"env_runners": {"num_env_steps_sampled_lifetime": 0}, "learners": {"__all_modules__": {"num_env_steps_trained_lifetime": 64}}},
    {"env_runners": {"num_env_steps_sampled_lifetime": 64}, "learners": {"__all_modules__": {"num_env_steps_trained_lifetime": float("nan")}}},
])
def test_counters_reject_missing_or_invalid_learning_evidence(result):
    with pytest.raises(RuntimeError, match="counter"):
        training_counters(result)


class FakeClock:
    def __init__(self):
        self.now = 0.0

    def monotonic(self):
        return self.now

    def sleep(self, delay):
        self.now += delay


def test_cleanup_queries_only_known_names_until_each_returns_404():
    clock = FakeClock()
    calls = []
    claims = [record["envs"][0]["claims"][0] for record in runner_records()]
    def lookup(namespace, name, request_timeout):
        calls.append((namespace, name, request_timeout))
        if name == "claim-2" or len(calls) > 2:
            raise ApiException(status=404)
        return {"metadata": {"name": name}}
    deleted = wait_for_claim_deletion(claims + claims, lookup, timeout_seconds=3,
                                     monotonic=clock.monotonic, sleep=clock.sleep)
    assert deleted == 2
    assert {call[:2] for call in calls} == {("training", "claim-1"), ("training", "claim-2")}
    assert all(0 < call[2] <= 3 for call in calls)


@pytest.mark.parametrize("failure", [None, ApiException(status=403), TimeoutError("request timeout")])
def test_cleanup_fails_boundedly_with_exact_remaining_names(failure):
    clock = FakeClock()
    def lookup(namespace, name, request_timeout):
        if failure:
            raise failure
        return {"metadata": {"name": name}}
    claim = runner_records()[0]["envs"][0]["claims"][0]
    with pytest.raises(RuntimeError, match="training/claim-1") as error:
        wait_for_claim_deletion([claim], lookup, timeout_seconds=2,
                                monotonic=clock.monotonic, sleep=clock.sleep)
    assert "kubectl" in str(error.value)
    assert clock.now <= 2


@pytest.fixture
def claim_http_api(request):
    requests = []
    stop = Event()
    connection_closed = Event()
    status = getattr(request, "param", None)

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self):
            requests.append({
                "path": self.path,
                "authorization": self.headers.get("Authorization"),
                "custom_header": self.headers.get("X-Test-Header"),
                "cookie": self.headers.get("Cookie"),
            })
            if status is None:
                # Hold the response until teardown so each attempt times out.
                stop.wait(5)
                return
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "2")
            if status == 503:
                self.send_header("Retry-After", "0")
            elif status == 302:
                self.send_header("Location", self.path)
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, *args):
            pass

        def finish(self):
            try:
                super().finish()
            finally:
                connection_closed.set()

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=lambda: server.serve_forever(poll_interval=0.01))
    thread.start()
    configuration = client.Configuration()
    configuration.host = f"http://127.0.0.1:{server.server_port}"
    configuration.proxy = None
    configuration.retries = 3
    configuration.api_key["authorization"] = "Bearer test-token"
    try:
        with client.ApiClient(configuration=configuration, cookie="test-cookie=1") as api_client:
            api_client.set_default_header("X-Test-Header", "claim-verification")
            try:
                yield client.CustomObjectsApi(api_client), requests, status, connection_closed
            finally:
                api_client.rest_client.pool_manager.clear()
    finally:
        stop.set()
        server.shutdown()
        server.server_close()
        thread.join()


def test_cleanup_request_timeout_does_not_retry_the_http_get(claim_http_api):
    api, requests, _, _ = claim_http_api
    claim = {"namespace": "training", "claim_name": "claim-1"}
    with claim_lookup(api) as lookup:
        with pytest.raises(RuntimeError, match="Claim GET failed.*training/claim-1"):
            wait_for_claim_deletion([claim], lookup, timeout_seconds=0.3)
    assert [request["path"] for request in requests] == [
        "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/training/sandboxclaims/claim-1",
    ]


@pytest.mark.parametrize("claim_http_api", [404, 403, 503, 302], indirect=True)
def test_cleanup_http_status_preserves_identity_without_retries(claim_http_api):
    api, requests, status, connection_closed = claim_http_api
    claim = {"namespace": "training", "claim_name": "claim-1"}
    default_retries = client.Configuration.get_default_copy().retries
    with claim_lookup(api) as lookup:
        # Only 404 proves absence; other responses remain failures.
        if status == 404:
            assert wait_for_claim_deletion([claim], lookup, timeout_seconds=1) == 1
        else:
            with pytest.raises(RuntimeError, match="Claim GET failed.*training/claim-1"):
                wait_for_claim_deletion([claim], lookup, timeout_seconds=1)
    assert len(requests) == 1
    assert requests[0]["authorization"] == "Bearer test-token"
    assert requests[0]["custom_header"] == "claim-verification"
    assert requests[0]["cookie"] == "test-cookie=1"
    assert api.api_client.configuration.retries == 3
    assert client.Configuration.get_default_copy().retries == default_retries
    assert connection_closed.wait(5), "cleanup client left its HTTP connection open"


@pytest.mark.parametrize("claim_http_api", [404], indirect=True)
def test_cleanup_client_closes_connections_when_verification_raises(claim_http_api):
    api, requests, _, connection_closed = claim_http_api
    with pytest.raises(ValueError, match="original verification failure"):
        with claim_lookup(api) as lookup:
            with pytest.raises(ApiException) as error:
                lookup("training", "claim-1", 1)
            assert error.value.status == 404
            raise ValueError("original verification failure")
    assert len(requests) == 1
    assert connection_closed.wait(5), "failed verification left its HTTP connection open"


def test_checkpoint_is_in_unique_writable_run_directory(tmp_path):
    first = prepare_run_directory(tmp_path, "run-one")
    second = prepare_run_directory(tmp_path, "run-two")
    checkpoint = first / "algorithm"
    checkpoint.mkdir()
    (checkpoint / "metadata.json").write_text("{}")
    assert first != second
    assert validate_checkpoint(checkpoint, first) == str(checkpoint)
    with pytest.raises(RuntimeError, match="checkpoint"):
        validate_checkpoint(second, first)
    empty = first / "empty"
    empty.mkdir()
    with pytest.raises(RuntimeError, match="checkpoint"):
        validate_checkpoint(empty, first)
