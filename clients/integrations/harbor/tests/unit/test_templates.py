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

"""Template and pool rendering, and the store's create-once contract."""

import pytest

from k8s_agent_sandbox_harbor.labels import LABEL_MANAGED_BY, MANAGED_BY_VALUE
from k8s_agent_sandbox_harbor.options import EnvironmentOptions
from k8s_agent_sandbox_harbor.templates import (CLAIMS, POOLS, TEMPLATES, ResourceSpec,
                                                TemplateStore, cpu_quantity, render_pool,
                                                render_template, template_name_for)

from .fake_k8s import FakeCustomObjects

RES = ResourceSpec(cpu_request="1", cpu_limit="1", memory_request="2048Mi", memory_limit="2048Mi",
                   storage_request="10240Mi")


def test_cpu_quantity():
    assert cpu_quantity(1) == "1" and cpu_quantity(2.0) == "2" and cpu_quantity(0.5) == "500m"


def test_render_template_pod_spec_and_network():
    opts = EnvironmentOptions(runtime_class="gvisor", node_selector={"pool": "sb"},
                              image_pull_secret="ps", extra_labels={"team": "rl"},
                              tolerations=[{"key": "sandbox", "operator": "Exists"}])
    t = render_template(name="hb-abc", namespace="harbor", image="alexgshaw/x:1", resources=RES,
                        options=opts, workdir="/app", environment_name="x", environment_id="e1")
    pod = t["spec"]["podTemplate"]["spec"]
    c = pod["containers"][0]
    assert t["kind"] == "SandboxTemplate" and t["metadata"]["labels"][LABEL_MANAGED_BY] == MANAGED_BY_VALUE
    assert t["metadata"]["labels"]["team"] == "rl" and t["metadata"]["annotations"]["harbor.io/image"] == "alexgshaw/x:1"
    assert c["name"] == "main" and c["image"] == "alexgshaw/x:1" and c["command"] == ["sleep", "infinity"]
    assert c["workingDir"] == "/app" and c["securityContext"] == {"runAsUser": 0, "runAsGroup": 0}
    assert c["resources"] == {"requests": {"cpu": "1", "memory": "2048Mi", "ephemeral-storage": "10240Mi"},
                              "limits": {"cpu": "1", "memory": "2048Mi"}}
    assert pod["automountServiceAccountToken"] is False and pod["runtimeClassName"] == "gvisor"
    assert pod["nodeSelector"] == {"pool": "sb"} and pod["imagePullSecrets"] == [{"name": "ps"}]
    assert pod["tolerations"] == [{"key": "sandbox", "operator": "Exists"}]
    egress = t["spec"]["networkPolicy"]["egress"]
    assert t["spec"]["networkPolicy"]["ingress"] == []
    assert egress[0]["ports"] == [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]
    assert "10.0.0.0/8" in egress[1]["to"][0]["ipBlock"]["except"]


def test_managed_default_network_and_no_root_leave_fields_out():
    opts = EnvironmentOptions(network="managed-default", run_as_root=False)
    t = render_template(name="n", namespace="ns", image="i", resources=ResourceSpec(), options=opts,
                        workdir=None, environment_name="x", environment_id="e")
    pod = t["spec"]["podTemplate"]["spec"]
    assert "networkPolicy" not in t["spec"] and "securityContext" not in pod
    assert "resources" not in pod["containers"][0] and "workingDir" not in pod["containers"][0]


def test_template_name_tracks_the_rendered_spec():
    opts = EnvironmentOptions()
    a = template_name_for(opts, "img:1", RES, "/app")
    assert a == template_name_for(opts, "img:1", RES, "/app") and a.startswith("hb-")
    assert a != template_name_for(opts, "img:2", RES, "/app")
    assert a != template_name_for(opts, "img:1", ResourceSpec(cpu_request="2"), "/app")
    assert a != template_name_for(EnvironmentOptions(runtime_class="gvisor"), "img:1", RES, "/app")


def test_render_pool():
    p = render_pool(name="hb-abc", namespace="ns", template_name="hb-abc", replicas=3, labels={"a": "b"})
    assert p["kind"] == "SandboxWarmPool" and p["spec"] == {"replicas": 3, "sandboxTemplateRef": {"name": "hb-abc"}}


async def test_store_creates_once_tolerates_409_and_cleans_up():
    api = FakeCustomObjects()
    opts = EnvironmentOptions(namespace="ns", warm_replicas=1)
    store = TemplateStore(api, "ns", opts)
    name = await store.ensure(image="img:1", resources=RES, workdir=None, environment_name="t",
                              environment_id="e")
    assert await store.ensure(image="img:1", resources=RES, workdir=None, environment_name="t",
                              environment_id="e") == name
    assert [c for c in api.calls if c[0] == "create"] == [("create", TEMPLATES, name), ("create", POOLS, name)]
    assert api.objects[(POOLS, name)]["spec"]["replicas"] == 1

    # Another process created the pair first: 409 on both is success.
    other = TemplateStore(api, "ns", opts)
    assert await other.ensure(image="img:1", resources=RES, workdir=None, environment_name="t",
                              environment_id="e") == name

    api.objects[(CLAIMS, "c1")] = {"metadata": {"name": "c1", "labels": {LABEL_MANAGED_BY: MANAGED_BY_VALUE}}}
    api.objects[(CLAIMS, "foreign")] = {"metadata": {"name": "foreign", "labels": {}}}
    counts = await store.delete_managed()
    assert counts == {CLAIMS: 1, POOLS: 1, TEMPLATES: 1}
    assert list(api.objects) == [(CLAIMS, "foreign")]


async def test_store_raises_on_other_api_errors():
    class Broken(FakeCustomObjects):
        async def create_namespaced_custom_object(self, **kw):
            from .fake_k8s import ApiError
            raise ApiError(403, "Forbidden")

    with pytest.raises(Exception, match="403"):
        await TemplateStore(Broken(), "ns", EnvironmentOptions()).ensure(
            image="img", resources=RES, workdir=None, environment_name="t", environment_id="e")
