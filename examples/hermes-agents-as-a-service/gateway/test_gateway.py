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
Unit tests for the hermes-agents-as-a-service gateway example.

gateway.py runs as the control+data plane in front of agent-sandbox; these
tests cover its security/parsing logic (bearer-token auth, DNS-1123 user
validation, the mode x conditions state machine) and the /users and
idle-sweeper decision logic, with the Kubernetes CustomObjectsApi mocked.
No kind cluster or container build is needed.

gateway.py isn't a valid module name to import as a package member from a
test file that also wants a fresh module per test (its top level calls
config.load_incluster_config()/load_kube_config() and reads
os.environ["API_SERVER_KEY"], both of which would blow up or reach a real
cluster), so each test loads its own copy by file path via the `gw`
fixture, mirroring examples/hpa-swp-scaling/test_create_claim.py.
"""

import hashlib
import importlib.util
import json
import os
import time
from types import SimpleNamespace
from unittest import mock

import pytest
from kubernetes import client as k8s_client
from kubernetes.config import ConfigException

GATEWAY_PATH = os.path.join(os.path.dirname(__file__), "gateway.py")


class _StopLoop(Exception):
    """Sentinel used to break idle_sweeper()'s `while True` after one pass."""


def _load_gateway(monkeypatch, mock_crd):
    monkeypatch.setenv("API_SERVER_KEY", "test-server-key")
    monkeypatch.setenv("NAMESPACE", "test-ns")
    monkeypatch.setenv("POOL", "test-pool")
    monkeypatch.setenv("IDLE_TIMEOUT", "60")
    monkeypatch.setenv("WAKE_TIMEOUT", "120")

    with mock.patch("kubernetes.config.load_incluster_config",
                     side_effect=ConfigException("not running in a cluster")), \
         mock.patch("kubernetes.config.load_kube_config"), \
         mock.patch("kubernetes.client.CustomObjectsApi", return_value=mock_crd):
        spec = importlib.util.spec_from_file_location("hermes_gateway", GATEWAY_PATH)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


@pytest.fixture
def gw(monkeypatch):
    """A freshly loaded gateway module (own last_activity/in_flight state)
    with its Kubernetes CustomObjectsApi mocked, plus a Flask test client."""
    mock_crd = mock.MagicMock()
    module = _load_gateway(monkeypatch, mock_crd)
    module.app.testing = True  # propagate unhandled exceptions instead of a bare 500
    return SimpleNamespace(module=module, crd=mock_crd, client=module.app.test_client())


def crd_get_side_effect(claims=None, sandboxes=None):
    """Builds a get_namespaced_custom_object side_effect that serves
    "sandboxclaims" from `claims` and "sandboxes" from `sandboxes` (both
    {name: obj_or_None} maps), raising a 404 ApiException for anything
    missing -- mirroring how get_claim()/get_sandbox() interpret 404."""
    claims = claims or {}
    sandboxes = sandboxes or {}

    def _get(group, version, namespace, plural, name):
        if plural == "sandboxclaims":
            table = claims
        elif plural == "sandboxes":
            table = sandboxes
        else:
            raise AssertionError(f"unexpected plural {plural!r}")
        obj = table.get(name)
        if obj is None:
            raise k8s_client.ApiException(status=404)
        return obj

    return _get


# --- condition() ---------------------------------------------------------

def test_condition_true_when_status_matches(gw):
    obj = {"status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    assert gw.module.condition(obj, "Ready") is True


def test_condition_false_when_status_is_false(gw):
    obj = {"status": {"conditions": [{"type": "Ready", "status": "False"}]}}
    assert gw.module.condition(obj, "Ready") is False


def test_condition_false_when_type_absent(gw):
    obj = {"status": {"conditions": [{"type": "Suspended", "status": "True"}]}}
    assert gw.module.condition(obj, "Ready") is False


def test_condition_false_when_no_status(gw):
    assert gw.module.condition({}, "Ready") is False


# --- derive_state() --------------------------------------------------------

def test_derive_state_provisioning_when_sandbox_missing(gw):
    assert gw.module.derive_state(None) == "Provisioning"


def test_derive_state_ready(gw):
    sandbox = {"spec": {}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    assert gw.module.derive_state(sandbox) == "Ready"


def test_derive_state_waking_when_not_yet_ready(gw):
    sandbox = {"spec": {"operatingMode": "Running"}, "status": {"conditions": []}}
    assert gw.module.derive_state(sandbox) == "Waking"


def test_derive_state_suspended(gw):
    sandbox = {"spec": {"operatingMode": "Suspended"},
               "status": {"conditions": [{"type": "Suspended", "status": "True"}]}}
    assert gw.module.derive_state(sandbox) == "Suspended"


def test_derive_state_suspending_while_mode_flipped_but_not_settled(gw):
    sandbox = {"spec": {"operatingMode": "Suspended"}, "status": {"conditions": []}}
    assert gw.module.derive_state(sandbox) == "Suspending"


# --- authorized() ----------------------------------------------------------

def _claim_for_token(module, token):
    return {"metadata": {"annotations": {
        module.TOKEN_ANNOTATION: hashlib.sha256(token.encode()).hexdigest()}}}


def test_authorized_accepts_correct_bearer_token(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    with gw.module.app.test_request_context(headers={"Authorization": "Bearer s3cret"}):
        assert gw.module.authorized(claim) is True


def test_authorized_rejects_wrong_token(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    with gw.module.app.test_request_context(headers={"Authorization": "Bearer wrong"}):
        assert gw.module.authorized(claim) is False


def test_authorized_rejects_missing_header(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    with gw.module.app.test_request_context():
        assert gw.module.authorized(claim) is False


def test_authorized_rejects_empty_bearer_token(gw):
    claim = _claim_for_token(gw.module, "")
    with gw.module.app.test_request_context(headers={"Authorization": "Bearer "}):
        # bool(token) guards against an empty token matching an empty/unset
        # annotation hash.
        assert gw.module.authorized(claim) is False


# --- POST /users (create_user) ---------------------------------------------

@pytest.mark.parametrize("bad_user", ["Alice", "-alice", "alice-", "", "a_b", 123, None])
def test_create_user_rejects_invalid_dns1123_label(gw, bad_user):
    response = gw.client.post("/users", json={"user": bad_user})
    assert response.status_code == 400
    gw.crd.create_namespaced_custom_object.assert_not_called()


def test_create_user_rejects_label_over_max_len(gw):
    too_long = "a" * (gw.module.MAX_USER_LEN + 1)
    response = gw.client.post("/users", json={"user": too_long})
    assert response.status_code == 400
    gw.crd.create_namespaced_custom_object.assert_not_called()


def test_create_user_conflict_when_claim_already_exists(gw):
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": {"metadata": {"name": "hermes-alice"}}})

    response = gw.client.post("/users", json={"user": "alice"})

    assert response.status_code == 409
    gw.crd.create_namespaced_custom_object.assert_not_called()


def test_create_user_conflict_on_concurrent_signup_race(gw):
    gw.crd.get_namespaced_custom_object.side_effect = k8s_client.ApiException(status=404)
    gw.crd.create_namespaced_custom_object.side_effect = k8s_client.ApiException(status=409)

    response = gw.client.post("/users", json={"user": "alice"})

    assert response.status_code == 409


def test_create_user_success_stores_only_token_hash(gw, monkeypatch):
    gw.crd.get_namespaced_custom_object.side_effect = k8s_client.ApiException(status=404)
    ready_sandbox = {
        "metadata": {"name": "hermes-alice-sbx"},
        "spec": {},
        "status": {"conditions": [{"type": "Ready", "status": "True"}]},
    }
    # wait_ready() polls with time.sleep(1); stub it out rather than racing
    # a real poll loop in a unit test.
    monkeypatch.setattr(gw.module, "wait_ready", lambda user, timeout: ready_sandbox)

    response = gw.client.post("/users", json={"user": "alice"})

    assert response.status_code == 201
    body = response.get_json()
    assert body["user"] == "alice"
    assert body["state"] == "Ready"
    assert body["sandbox"] == "hermes-alice-sbx"
    assert body["token"]  # shown once in the response...

    gw.crd.create_namespaced_custom_object.assert_called_once()
    claim_body = gw.crd.create_namespaced_custom_object.call_args.args[4]
    assert claim_body["metadata"]["name"] == "hermes-alice"
    # ...but only its SHA-256 is ever sent to the API server.
    want_hash = hashlib.sha256(body["token"].encode()).hexdigest()
    assert claim_body["metadata"]["annotations"][gw.module.TOKEN_ANNOTATION] == want_hash
    assert body["token"] not in json.dumps(claim_body)
    assert claim_body["spec"]["warmPoolRef"]["name"] == "test-pool"
    assert claim_body["spec"]["additionalPodMetadata"]["labels"]["sandbox.users.io/hermes-user"] == "alice"


# --- GET /users/<user> and DELETE /users/<user> -----------------------------

def test_get_user_not_found(gw):
    gw.crd.get_namespaced_custom_object.side_effect = k8s_client.ApiException(status=404)
    response = gw.client.get("/users/alice")
    assert response.status_code == 404


def test_get_user_requires_valid_token(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim})

    response = gw.client.get("/users/alice", headers={"Authorization": "Bearer wrong"})

    assert response.status_code == 401


def test_get_user_returns_state_when_authorized(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    sandbox = {"metadata": {"name": "hermes-alice-sbx"}, "spec": {},
               "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    claim["status"] = {"sandbox": {"name": "hermes-alice-sbx"}}
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim}, sandboxes={"hermes-alice-sbx": sandbox})

    response = gw.client.get("/users/alice", headers={"Authorization": "Bearer s3cret"})

    assert response.status_code == 200
    assert response.get_json()["state"] == "Ready"


def test_delete_user_not_found(gw):
    gw.crd.get_namespaced_custom_object.side_effect = k8s_client.ApiException(status=404)
    response = gw.client.delete("/users/alice")
    assert response.status_code == 404


def test_delete_user_requires_valid_token(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim})

    response = gw.client.delete("/users/alice", headers={"Authorization": "Bearer wrong"})

    assert response.status_code == 401
    gw.crd.delete_namespaced_custom_object.assert_not_called()


def test_delete_user_cascades_and_clears_activity(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim})
    gw.module.last_activity["alice"] = time.time()

    response = gw.client.delete("/users/alice", headers={"Authorization": "Bearer s3cret"})

    assert response.status_code == 200
    gw.crd.delete_namespaced_custom_object.assert_called_once()
    assert "alice" not in gw.module.last_activity


def test_delete_user_already_gone_is_idempotent(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim})
    gw.crd.delete_namespaced_custom_object.side_effect = k8s_client.ApiException(status=404)

    response = gw.client.delete("/users/alice", headers={"Authorization": "Bearer s3cret"})

    assert response.status_code == 200


def test_delete_user_unexpected_api_error_propagates(gw):
    claim = _claim_for_token(gw.module, "s3cret")
    gw.crd.get_namespaced_custom_object.side_effect = crd_get_side_effect(
        claims={"hermes-alice": claim})
    gw.crd.delete_namespaced_custom_object.side_effect = k8s_client.ApiException(status=500)

    with pytest.raises(k8s_client.ApiException):
        gw.client.delete("/users/alice", headers={"Authorization": "Bearer s3cret"})


# --- idle_sweeper() ----------------------------------------------------------

def _run_one_sweep(gw):
    # time.sleep(15) runs before the try/except in idle_sweeper's loop body,
    # so the first call must succeed (to let one pass execute) and only the
    # second call breaks out.
    with mock.patch("time.sleep", side_effect=[None, _StopLoop()]):
        with pytest.raises(_StopLoop):
            gw.module.idle_sweeper()


def test_idle_sweeper_suspends_idle_ready_sandbox(gw):
    sandbox = {"metadata": {"name": "hermes-alice-sbx"}, "spec": {},
               "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    claim_item = {"metadata": {"name": "hermes-alice"},
                  "status": {"sandbox": {"name": "hermes-alice-sbx"}}}
    gw.crd.list_namespaced_custom_object.return_value = {"items": [claim_item]}
    gw.crd.get_namespaced_custom_object.return_value = sandbox
    gw.module.last_activity["alice"] = time.time() - gw.module.IDLE_TIMEOUT - 10

    _run_one_sweep(gw)

    gw.crd.patch_namespaced_custom_object.assert_called_once()
    args = gw.crd.patch_namespaced_custom_object.call_args.args
    assert args[4] == "hermes-alice-sbx"
    assert args[5] == {"spec": {"operatingMode": "Suspended"}}


def test_idle_sweeper_skips_user_with_in_flight_request(gw):
    sandbox = {"metadata": {"name": "hermes-alice-sbx"}, "spec": {},
               "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    claim_item = {"metadata": {"name": "hermes-alice"},
                  "status": {"sandbox": {"name": "hermes-alice-sbx"}}}
    gw.crd.list_namespaced_custom_object.return_value = {"items": [claim_item]}
    gw.crd.get_namespaced_custom_object.return_value = sandbox
    gw.module.last_activity["alice"] = time.time() - gw.module.IDLE_TIMEOUT - 10
    gw.module.in_flight["alice"] = 1

    _run_one_sweep(gw)

    gw.crd.patch_namespaced_custom_object.assert_not_called()


def test_idle_sweeper_skips_recently_active_user(gw):
    sandbox = {"metadata": {"name": "hermes-alice-sbx"}, "spec": {},
               "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    claim_item = {"metadata": {"name": "hermes-alice"},
                  "status": {"sandbox": {"name": "hermes-alice-sbx"}}}
    gw.crd.list_namespaced_custom_object.return_value = {"items": [claim_item]}
    gw.crd.get_namespaced_custom_object.return_value = sandbox
    gw.module.last_activity["alice"] = time.time()

    _run_one_sweep(gw)

    gw.crd.patch_namespaced_custom_object.assert_not_called()


def test_idle_sweeper_skips_sandbox_not_ready(gw):
    sandbox = {"metadata": {"name": "hermes-alice-sbx"}, "spec": {}, "status": {"conditions": []}}
    claim_item = {"metadata": {"name": "hermes-alice"},
                  "status": {"sandbox": {"name": "hermes-alice-sbx"}}}
    gw.crd.list_namespaced_custom_object.return_value = {"items": [claim_item]}
    gw.crd.get_namespaced_custom_object.return_value = sandbox
    gw.module.last_activity["alice"] = time.time() - gw.module.IDLE_TIMEOUT - 10

    _run_one_sweep(gw)

    gw.crd.patch_namespaced_custom_object.assert_not_called()
