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

"""Check the deployment contracts without starting Ray or Kubernetes."""

import shlex
from pathlib import Path
from typing import Any

import yaml


def manifests() -> list[dict[str, Any]]:
    return [
        document
        for path in sorted(Path(__file__).with_name("kuberay").glob("*.yaml"))
        for document in yaml.safe_load_all(path.read_text())
        if document is not None
    ]


def resource(kind: str) -> dict[str, Any]:
    matches = [document for document in manifests() if document["kind"] == kind]
    assert len(matches) == 1, f"Expected one {kind}, found {len(matches)}"
    return matches[0]


def test_only_ray_clients_receive_minimal_sandbox_permissions():
    documents = manifests()
    namespace = resource("Namespace")["metadata"]["name"]
    role = resource("Role")
    binding = resource("RoleBinding")
    service_accounts = {
        document["metadata"]["name"]: document
        for document in documents
        if document["kind"] == "ServiceAccount"
    }

    assert all(
        document["metadata"].get("namespace") == namespace
        for document in documents
        if document["kind"] != "Namespace"
    )
    permissions = {
        (group, kind, verb)
        for rule in role["rules"]
        for group in rule["apiGroups"]
        for kind in rule["resources"]
        for verb in rule["verbs"]
    }
    assert permissions == {
        ("extensions.agents.x-k8s.io", "sandboxclaims", "create"),
        ("extensions.agents.x-k8s.io", "sandboxclaims", "get"),
        ("extensions.agents.x-k8s.io", "sandboxclaims", "watch"),
        ("extensions.agents.x-k8s.io", "sandboxclaims", "delete"),
        ("agents.x-k8s.io", "sandboxes", "get"),
        ("agents.x-k8s.io", "sandboxes", "watch"),
    }
    assert binding["roleRef"] == {
        "apiGroup": "rbac.authorization.k8s.io",
        "kind": "Role",
        "name": role["metadata"]["name"],
    }
    assert binding["subjects"] == [
        {"kind": "ServiceAccount", "name": "rllib-sandbox", "namespace": namespace}
    ]
    assert set(service_accounts) == {"rllib-sandbox", "rllib-submitter"}
    assert service_accounts["rllib-submitter"]["automountServiceAccountToken"] is False
    assert not any(
        document["kind"] in {"ClusterRole", "ClusterRoleBinding"}
        for document in documents
    )


def test_warm_sandboxes_expose_only_direct_sandboxd_to_trusted_ray_pods():
    template = resource("SandboxTemplate")
    pool = resource("SandboxWarmPool")
    spec = template["spec"]
    pod = spec["podTemplate"]["spec"]
    container = pod["containers"][0]

    assert pool["spec"]["sandboxTemplateRef"]["name"] == template["metadata"]["name"]
    assert pool["spec"]["replicas"] >= 2
    assert spec["service"] is True
    ports = {port["containerPort"] for port in container["ports"]}
    assert ports == {8080, 9090}
    assert container["readinessProbe"]["httpGet"] == {
        "path": "/v1/health", "port": 8080
    }
    policy = spec["networkPolicy"]
    assert set(policy) == {"ingress", "egress"}
    assert policy["egress"] == []
    assert len(policy["ingress"]) == 1
    ingress = policy["ingress"][0]
    assert len(ingress["from"]) == 1
    peer = ingress["from"][0]
    assert set(peer) == {"podSelector"}
    assert peer["podSelector"]["matchLabels"] == {
        "app.kubernetes.io/part-of": "rllib-sandbox-example"
    }
    assert {(port["protocol"], port["port"]) for port in ingress["ports"]} == {
        ("TCP", port) for port in ports
    }
    assert pod["automountServiceAccountToken"] is False
    assert pod["securityContext"]["runAsNonRoot"] is True
    assert container["securityContext"]["allowPrivilegeEscalation"] is False
    assert container["securityContext"]["capabilities"]["drop"] == ["ALL"]
    assert container["image"].rsplit(":", 1)[-1] not in {"latest", "latest-main"}


def test_checkpoint_volume_has_one_head_writer_and_survives_raycluster_cleanup():
    pvc = resource("PersistentVolumeClaim")
    job_spec = resource("RayJob")["spec"]
    cluster = job_spec["rayClusterSpec"]
    head = cluster["headGroupSpec"]["template"]["spec"]
    checkpoint_name = pvc["metadata"]["name"]
    checkpoint_volumes = {
        volume["name"]
        for volume in head["volumes"]
        if volume.get("persistentVolumeClaim", {}).get("claimName") == checkpoint_name
    }

    assert pvc["spec"]["accessModes"] == ["ReadWriteOnce"]
    assert pvc["spec"]["resources"]["requests"]["storage"] == "1Gi"
    assert "storageClassName" not in pvc["spec"]
    assert not pvc["metadata"].get("ownerReferences")
    assert len(checkpoint_volumes) == 1
    assert any(
        mount["name"] in checkpoint_volumes and mount["mountPath"] == "/checkpoints"
        for mount in head["containers"][0]["volumeMounts"]
    )
    assert head["securityContext"]["runAsUser"] == 1000
    assert head["securityContext"]["runAsGroup"] == 100
    assert head["securityContext"]["fsGroup"] == 100
    assert head["securityContext"]["fsGroupChangePolicy"] == "OnRootMismatch"
    non_head_pods = [
        group["template"]["spec"] for group in cluster["workerGroupSpecs"]
    ] + [job_spec["submitterPodTemplate"]["spec"]]
    assert all(
        "persistentVolumeClaim" not in volume
        for pod in non_head_pods
        for volume in pod.get("volumes", [])
    )
    assert job_spec["shutdownAfterJobFinishes"] is True
    assert job_spec["ttlSecondsAfterFinished"] == 300


def test_rayjob_uses_the_same_image_and_identity_for_two_remote_runners():
    job = resource("RayJob")
    spec = job["spec"]
    cluster = spec["rayClusterSpec"]
    head = cluster["headGroupSpec"]
    workers = cluster["workerGroupSpecs"]
    submitter = spec["submitterPodTemplate"]
    ray_pods = [head["template"]] + [group["template"] for group in workers]
    role_binding = resource("RoleBinding")
    ray_service_account = role_binding["subjects"][0]["name"]
    allowed_labels = resource("SandboxTemplate")["spec"]["networkPolicy"][
        "ingress"
    ][0]["from"][0]["podSelector"]["matchLabels"]
    command = shlex.split(spec["entrypoint"])

    assert cluster["rayVersion"] == "2.58.0"
    assert cluster["enableInTreeAutoscaling"] is False
    assert head["rayStartParams"]["num-cpus"] == "0"
    assert len(workers) == 1
    assert (
        workers[0]["replicas"]
        == workers[0]["minReplicas"]
        == workers[0]["maxReplicas"]
        == 1
    )
    assert workers[0]["rayStartParams"]["num-cpus"] == "2"
    for pod in ray_pods:
        assert pod["spec"]["serviceAccountName"] == ray_service_account
        assert pod["spec"].get("automountServiceAccountToken", True) is True
        assert pod["spec"]["restartPolicy"] == "Never"
        assert allowed_labels.items() <= pod["metadata"]["labels"].items()
        container = pod["spec"]["containers"][0]
        assert container["resources"]["requests"]["cpu"] == "2"
    images = {
        pod["spec"]["containers"][0]["image"] for pod in ray_pods + [submitter]
    }
    assert len(images) == 1
    assert all(
        image.rsplit(":", 1)[-1] not in {"latest", "latest-main"}
        for image in images
    )
    assert submitter["spec"]["serviceAccountName"] != ray_service_account
    assert submitter["spec"]["automountServiceAccountToken"] is False
    assert submitter["spec"]["restartPolicy"] == "Never"
    assert "command" not in submitter["spec"]["containers"][0]
    assert not any(
        key in submitter.get("metadata", {}).get("labels", {})
        for key in allowed_labels
    )
    assert "--verify-run" in command
    expected_arguments = {
        "--connection-mode": "sandboxd-in-cluster",
        "--checkpoint-dir": "/checkpoints",
        "--ray-address": "auto",
        "--namespace": job["metadata"]["namespace"],
        "--warmpool": resource("SandboxWarmPool")["metadata"]["name"],
        "--num-env-runners": "2",
    }
    for flag, value in expected_arguments.items():
        assert command[command.index(flag) + 1] == value
    assert spec["submissionMode"] == "K8sJobMode"
    assert spec["backoffLimit"] == spec["submitterConfig"]["backoffLimit"] == 0
    assert not {
        "entrypointNumCpus",
        "entrypointNumGpus",
        "entrypointResources",
        "runtimeEnvYAML",
    }.intersection(spec)
