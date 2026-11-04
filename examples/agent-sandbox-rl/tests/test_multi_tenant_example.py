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

"""The load-bearing parts of examples/multi_tenant: the run id and config a
user gets, the quota fit, the runner over FakeFleet, the report round trip,
and the tenant manifest rendering. No cluster."""

import importlib.util
import json
import pathlib
import re

import pytest
import yaml

from agent_sandbox_rl import FleetConfig, constants
from agent_sandbox_rl.testing import FakeFleet

EXAMPLE = pathlib.Path(__file__).resolve().parent.parent / "examples" / "multi_tenant"


def _load(name, path):
  spec = importlib.util.spec_from_file_location(name, path)
  mod = importlib.util.module_from_spec(spec)
  spec.loader.exec_module(mod)
  return mod


run_experiment = _load("mt_run_experiment", EXAMPLE / "run_experiment.py")
run_many = _load("mt_run_many", EXAMPLE / "run_many.py")
render = _load("mt_render", EXAMPLE / "admin" / "render.py")

DNS_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
IMAGES = ["debian:bookworm-slim", "python:3.12-slim", "python:3.11-slim"]


# --- run id and config ------------------------------------------------------ #

def test_name_prefix_names_user_and_experiment():
  prefix = run_experiment.name_prefix_for("Alice Smith", "exp 1")
  assert prefix == "alice-smith-exp-1-"
  FleetConfig(template_name_prefix=prefix, run_isolation="names")   # the SDK agrees
  # Long names are cut, so the prefix leaves room for the run id and the hash.
  long = run_experiment.name_prefix_for("u" * 80, "e" * 80)
  assert len(long) <= 40 and DNS_LABEL.match(long.rstrip("-"))
  FleetConfig(template_name_prefix=long, run_isolation="names")
  with pytest.raises(ValueError):
    run_experiment.name_prefix_for("***", "exp1")


def test_tenant_namespace_uses_the_name_as_given():
  assert run_experiment.tenant_namespace("alice") == "rl-alice"
  assert run_experiment.tenant_namespace("a" * 60) == "rl-" + "a" * 60
  # The admin's scripts never transform the name, so neither does the runner.
  for bad in ("Alice", "alice smith", "a" * 61, "-alice", ""):
    with pytest.raises(ValueError):
      run_experiment.tenant_namespace(bad)
  with pytest.raises(ValueError):
    run_experiment.build_config("Alice", "exp1")
  assert run_experiment.report_path("reports", "alice", "exp 1") == \
      pathlib.Path("reports/alice/exp-1/report.json")


def test_config_stays_in_the_users_namespace():
  cfg = run_experiment.build_config("alice", "exp1", max_concurrent=3, cpu="200m",
                                    memory="256Mi")
  assert [c.namespace for c in cfg.clusters] == ["rl-alice"]
  assert cfg.run_isolation == "names"
  assert cfg.template_name_prefix == "alice-exp1-"
  # No caller-set run id: that would mean a shared job and would turn off the
  # circuit breaker and verify_teardown.
  assert cfg.run_id is None
  assert cfg.labels[constants.MANAGED_BY_LABEL] == constants.MANAGED_BY_VALUE
  assert cfg.labels[run_experiment.TENANT_LABEL] == "alice"
  assert cfg.labels[run_experiment.EXPERIMENT_LABEL] == "exp1"
  assert cfg.max_concurrent == 3 and cfg.max_live_sandboxes is None
  assert run_experiment.build_config("alice", "exp1", pod_budget=16).max_live_sandboxes == 16
  assert cfg.template.network_policy_management == "Unmanaged"
  # Released sandboxes must leave the quota quickly: sleep ignores SIGTERM.
  assert cfg.template.extra_pod_spec == {"terminationGracePeriodSeconds": 1}
  assert (cfg.template.resources.cpu, cfg.template.resources.memory) == ("200m", "256Mi")
  # Labels that land on pods must not use a controller-reserved prefix.
  for key in (run_experiment.TENANT_LABEL, run_experiment.EXPERIMENT_LABEL):
    assert not key.startswith(constants.RESERVED_POD_LABEL_PREFIXES)


def test_several_clusters_share_the_namespace_and_the_cluster_names():
  # Tenant kubeconfigs name contexts rl-<user>@<ctx>; the cluster name is the
  # admin's <ctx>, the same for every user, so image-affinity converges.
  assert run_experiment.cluster_name_for("rl-alice@prod-a") == "prod-a"
  assert run_experiment.cluster_name_for("rl-bob@prod-a") == "prod-a"
  assert run_experiment.cluster_name_for("kind-x") == "kind-x"
  assert run_experiment.cluster_name_for(None) == "default"
  cfg = run_experiment.build_config(
      "alice", "exp1", contexts=["rl-alice@a", "rl-alice@b"], kubeconfig="/k",
      pod_budget=14, cluster_budgets={"a": 8, "b": 6})
  assert [(c.name, c.context, c.kubeconfig, c.namespace, c.max_replicas) for c in cfg.clusters] == [
      ("a", "rl-alice@a", "/k", "rl-alice", 8), ("b", "rl-alice@b", "/k", "rl-alice", 6)]
  assert cfg.max_live_sandboxes == 14 and cfg.placement == "image-affinity"
  with pytest.raises(ValueError):            # two contexts, one cluster name
    run_experiment.build_config("alice", "exp1", contexts=["rl-alice@a", "a"])


def test_image_affinity_sends_every_user_to_the_same_cluster_per_image():
  from agent_sandbox_rl import ClusterRegistry, ImageAffinity
  from conftest import FakeCluster
  alice = ClusterRegistry([FakeCluster("a", "rl-alice"), FakeCluster("b", "rl-alice")])
  bob = ClusterRegistry([FakeCluster("b", "rl-bob"), FakeCluster("a", "rl-bob")])
  for img in IMAGES + ["node:22-slim", "ruby:3.3-slim"]:
    assert ImageAffinity().select(img, alice).name == ImageAffinity().select(img, bob).name


def test_fake_fleet_names_carry_user_experiment_and_run():
  fleet = FakeFleet(run_experiment.build_config("alice", "exp1"))
  fleet.load_tasks(IMAGES)
  plan = fleet.plan()
  assert fleet.namespace == "rl-alice"
  assert plan.entries
  for e in plan.entries:
    assert e.template.startswith("alice-exp1-") and fleet.run_id in e.template
    assert e.pool.startswith("pool-alice-exp1-") and fleet.run_id in e.pool


# --- quota fit --------------------------------------------------------------- #

COUNTS = {f"img{i}": 2 for i in range(10)}          # ten images, two tasks each


def test_footprint_follows_the_sdk_window():
  # One replica per image here, so the window is max_concurrent pools and the
  # peak adds a claimed pod and a refilling pod per slot.
  assert run_experiment.footprint(COUNTS, 3, 4) == (3, 9)
  assert run_experiment.footprint(COUNTS, 8, 4) == (8, 24)
  assert run_experiment.footprint(COUNTS, 1, 4) == (1, 3)
  fleet = FakeFleet(run_experiment.build_config("alice", "exp1", max_concurrent=3))
  fleet.load_tasks([img for img in COUNTS for _ in range(2)])
  fleet.plan()
  assert fleet.recommended_window() == run_experiment.footprint(COUNTS, 3, 4)[0]


@pytest.mark.parametrize("hard,used,expect", [
    ({}, {}, None),                                              # no quota
    ({"pods": "16"}, {}, 16),
    ({"pods": "16"}, {"pods": "10"}, 6),                         # what is left
    ({"requests.cpu": "1"}, {}, 10),                             # 100m per pod
    ({"requests.memory": "1Gi"}, {}, 8),                         # 128Mi per pod
    ({"pods": "16", "requests.cpu": "500m"}, {}, 5),             # the tightest wins
])
def test_pod_budget(hard, used, expect):
  assert run_experiment.pod_budget(hard, used, cpu="100m", memory="128Mi") == expect


@pytest.mark.parametrize("hard,used,wanted,share,expect", [
    ({}, {}, 8, 1, (8, None)),                     # no quota: as asked
    ({"pods": "16"}, {}, 8, 1, (5, 16)),           # window 5 + 10 = 15 fits, 6 does not
    ({"pods": "16"}, {}, 3, 1, (3, 16)),           # already fits
    ({"pods": "16"}, {}, 8, 2, (2, 8)),            # two experiments at once: half each
    ({"pods": "16"}, {"pods": "10"}, 4, 1, (2, 6)),
    ({"pods": "2"}, {}, 4, 1, (0, 2)),             # not even one slot (needs 3 pods)
])
def test_fit_to_quota(hard, used, wanted, share, expect):
  assert run_experiment.fit_to_quota(hard, used, cpu="100m", memory="128Mi", counts=COUNTS,
                                     wanted=wanted, max_pool=4, share=share) == expect


# --- the runner over the fake ------------------------------------------------ #

def test_drive_runs_every_task_and_tears_down():
  fleet = FakeFleet(run_experiment.build_config("alice", "exp1"),
                    exec_outputs={"os-release": "PRETTY_NAME=x\nhost"})
  out = run_experiment.drive(fleet, IMAGES, tasks_per_image=2)
  assert out["run_id"] == fleet.run_id
  assert len(out["results"]) == 6
  assert all(isinstance(r, dict) and r["output"] for r in out["results"])
  assert {r["image"] for r in out["results"]} == set(IMAGES)
  assert len({r["task"] for r in out["results"]}) == 6
  assert run_experiment.claims_by_cluster(out["results"]) == {"fake": 6}
  assert fleet.teardowns == 1
  assert out["report"]["tasks_ok"] == 6 and out["report"]["tasks_err"] == 0
  assert out["verify_teardown"] == {}            # the fake has no reaper
  assert run_experiment.teardown_clean(out["verify_teardown"])


def test_teardown_clean_reads_what_remained_after_the_reaper():
  swept = {"c": {"claims": 0, "warmpools": 0, "templates": 0, "pods": 3,
                 "remaining": {"claims": 0, "warmpools": 0, "templates": 0, "pods": 0}}}
  # Pods the controller was still removing are swept and reported, not leftovers.
  assert run_experiment.teardown_clean(swept)
  assert run_experiment.reaper_found(swept) == {"claims": 0, "warmpools": 0,
                                                "templates": 0, "pods": 3}
  assert not run_experiment.teardown_clean(
      {"c": {**swept["c"], "remaining": {**swept["c"]["remaining"], "claims": 1}}})
  assert not run_experiment.teardown_clean({"c": {"skipped": "shared run id"}})
  assert not run_experiment.teardown_clean({"c": {"error": "boom"}})
  assert run_experiment.teardown_clean({})                 # fakes have no reaper


def test_report_round_trip(tmp_path):
  fleet = FakeFleet(run_experiment.build_config("bob", "exp2"))
  out = run_experiment.drive(fleet, IMAGES, tasks_per_image=1)
  out["results"][0] = RuntimeError("task blew up")       # one captured failure
  path = run_experiment.write_report(run_experiment.report_path(tmp_path, "bob", "exp2"),
                                     user="bob", experiment="exp2", outcome=out,
                                     max_concurrent=4)
  assert path == tmp_path / "bob" / "exp2" / "report.json"
  doc = json.loads(path.read_text())
  assert (doc["user"], doc["experiment"], doc["namespace"]) == ("bob", "exp2", "rl-bob")
  assert doc["run_id"] == fleet.run_id
  assert doc["tasks"] == 3 and doc["task_errors"] == 1
  assert doc["results"][0] == {"error": "task blew up"}
  assert doc["claims_by_cluster"] == {"fake": 2}       # the failed task has no cluster
  rows = run_many.summarize([doc, {**doc, "user": "alice", "experiment": "exp1"}])
  assert [(r["user"], r["experiment"]) for r in rows] == [("alice", "exp1"), ("bob", "exp2")]
  assert rows[1]["claims"] == 3 and rows[1]["task_errors"] == 1 and rows[1]["teardown_clean"]
  table = run_many.render_table(rows)
  assert table.splitlines()[0].startswith("user") and "rl-bob" in table
  assert "tasks per cluster" not in table               # one cluster: no column
  two = {**doc, "claims_by_cluster": {"a": 2, "b": 1}}
  table = run_many.render_table(run_many.summarize([two]))
  assert "tasks per cluster" in table and "a=2 b=1" in table


def test_run_many_refuses_bad_or_repeated_users():
  for users in ("alice,alice", "Alice", "bob,alice,bob"):
    with pytest.raises(SystemExit):
      run_many.main(["--users", users, "--experiments", "1", "--report-dir", "/nonexistent"])


# --- the tenant manifest ----------------------------------------------------- #

VALUES = {"TENANT": "alice", "TENANT_USER": "alice@example.com",
          "CONTROLLER_NAMESPACE": "agent-sandbox-system", "QUOTA_PODS": "16",
          "QUOTA_CPU": "4", "QUOTA_MEMORY": "8Gi", "QUOTA_CLAIMS": "16",
          "LIMIT_MAX_CPU": "2", "LIMIT_MAX_MEMORY": "4Gi"}


def test_render_refuses_a_missing_value():
  text = (EXAMPLE / "admin" / "tenant.yaml").read_text()
  with pytest.raises(KeyError, match="QUOTA_PODS"):
    render.render(text, {k: v for k, v in VALUES.items() if k != "QUOTA_PODS"})
  for bad in ("Alice", "a_b", "a" * 61):
    with pytest.raises(ValueError):
      render.check_name(bad, "tenant", 60)
  assert render.check_name("a" * 60, "tenant", 60) == "a" * 60


def test_tenant_manifest_renders_one_isolated_tenant():
  text = render.render((EXAMPLE / "admin" / "tenant.yaml").read_text(), VALUES)
  assert "${" not in text
  docs = {(d["kind"], d["metadata"]["name"]): d for d in yaml.safe_load_all(text)}
  ns = docs[("Namespace", "rl-alice")]
  assert ns["metadata"]["labels"]["agent-sandbox-rl/tenant"] == "alice"
  quota = docs[("ResourceQuota", "rl-tenant")]
  assert quota["metadata"]["namespace"] == "rl-alice"
  assert quota["spec"]["hard"] == {
      "pods": "16", "requests.cpu": "4", "requests.memory": "8Gi",
      "count/sandboxclaims.extensions.agents.x-k8s.io": "16"}
  limits = docs[("LimitRange", "rl-tenant")]["spec"]["limits"][0]
  # The SDK sets requests and no limits, so the default limit must admit any
  # request the tenant maximum allows.
  assert limits["default"] == limits["max"] == {"cpu": "2", "memory": "4Gi"}
  binding = docs[("RoleBinding", "rl-tenant")]
  assert {(s["kind"], s["name"]) for s in binding["subjects"]} == {
      ("ServiceAccount", "rl-tenant"), ("User", "alice@example.com")}
  assert ("ClusterRoleBinding", "rl-tenant-cluster-read-alice") in docs
  # Every namespaced object is in the tenant's namespace, except the two that
  # grant the controller-deployment read in the controller's namespace.
  for (kind, name), d in docs.items():
    if kind in ("Namespace", "ClusterRoleBinding"):
      continue
    expect = "agent-sandbox-system" if "controller-view" in name else "rl-alice"
    assert d["metadata"]["namespace"] == expect, (kind, name)
  # The Role must cover what the SDK and the reaper do in the namespace.
  rules = docs[("Role", "rl-tenant")]["rules"]
  def verbs(group, resource):
    for r in rules:
      if group in r["apiGroups"] and resource in r["resources"]:
        return set(r["verbs"])
    return set()
  assert {"create", "patch", "delete", "list"} <= verbs("extensions.agents.x-k8s.io", "sandboxclaims")
  assert {"create", "patch", "delete"} <= verbs("extensions.agents.x-k8s.io", "sandboxwarmpools")
  assert {"delete", "deletecollection", "list"} <= verbs("", "pods")
  assert "create" in verbs("", "pods/exec")
  assert "get" in verbs("", "pods/status")
  assert "get" in verbs("", "resourcequotas")
  assert "daemonsets" not in str(rules)           # pre-pull is the admin's job
  # Every pod in the namespace: a tenant's hand-made template may omit the
  # SDK's pod label, and its pods must not fall outside the policy.
  policy = docs[("NetworkPolicy", "agent-sandbox-rl-fleet")]
  assert policy["spec"]["podSelector"] == {}
  assert set(policy["spec"]["policyTypes"]) == {"Ingress", "Egress"}
