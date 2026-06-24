#!/usr/bin/env python3
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

"""Migration test script to validate the v1alpha1 -> v1beta1 upgrade path.

Supports both kubectl-based and Helm-based upgrade paths.
"""

import argparse
import json
import os
import subprocess
import sys
import time

# Add repo root to path to load shared utilities
_self_dir = os.path.dirname(os.path.abspath(__file__))
_repo_root = os.path.dirname(os.path.dirname(_self_dir))
if _repo_root not in sys.path:
    sys.path.insert(0, _repo_root)

from dev.tools.shared import utils as tools_utils


V1ALPHA1_RESOURCES = """apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: upgrade-template
  namespace: default
spec:
  podTemplate:
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: upgrade-template-cold
  namespace: default
spec:
  podTemplate:
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: upgrade-pool
  namespace: default
spec:
  replicas: 0
  sandboxTemplateRef:
    name: upgrade-template
---
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: upgrade-sandbox
  namespace: default
spec:
  replicas: 0 # v1alpha1 syntax (converts to operatingMode: Suspended)
  podTemplate:
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: upgrade-sandbox-running
  namespace: default
spec:
  replicas: 1 # v1alpha1 syntax (converts to operatingMode: Running)
  podTemplate:
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: upgrade-claim
  namespace: default
spec:
  sandboxTemplateRef:
    name: upgrade-template-cold
  warmpool: "default" # v1alpha1 syntax (converts to warmPoolRef.name: shadow-pool-upgrade-template-cold)
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: upgrade-claim-specific
  namespace: default
spec:
  sandboxTemplateRef:
    name: upgrade-template
  warmpool: "upgrade-pool" # v1alpha1 syntax (converts to warmPoolRef.name: upgrade-pool)
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: upgrade-claim-none
  namespace: default
spec:
  sandboxTemplateRef:
    name: upgrade-template-cold
  warmpool: "none" # v1alpha1 syntax (converts to warmPoolRef.name: shadow-pool-upgrade-template-cold)
---
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: upgrade-pool-warm-a1b2c
  namespace: default
  labels:
    agents.x-k8s.io/warmpool: upgrade-pool-warm
spec:
  replicas: 1
  podTemplate:
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: upgrade-pool-warm
  namespace: default
spec:
  replicas: 1
  sandboxTemplateRef:
    name: upgrade-template
---
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: upgrade-claim-warm
  namespace: default
spec:
  sandboxTemplateRef:
    name: upgrade-template
  warmpool: "default"
status:
  sandbox:
    name: upgrade-pool-warm-a1b2c
"""

def _safe_extract(tar, dest, members=None):
    base = os.path.realpath(dest)
    selected = members if members is not None else tar.getmembers()
    for member in selected:
        target = os.path.realpath(os.path.join(dest, member.name))
        if target != base and not target.startswith(base + os.sep):
            raise ValueError(f"Unsafe tar member path: {member.name}")
    tar.extractall(path=dest, members=selected)

def run_cmd(cmd, check=True, text=True, input_data=None, capture_output=False):
    """Executes a CLI command and prints/returns output."""
    cmd_str = " ".join(cmd) if isinstance(cmd, list) else cmd
    print(f"+ {cmd_str}")
    
    stdout_dest = subprocess.PIPE if capture_output else sys.stdout
    try:
        res = subprocess.run(
            cmd,
            check=check,
            stdout=stdout_dest,
            stderr=subprocess.PIPE,
            text=text,
            input=input_data,
            cwd=_repo_root
        )
        if res.stderr:
            print(res.stderr, file=sys.stderr)
        return res
    except subprocess.CalledProcessError as e:
        if e.stderr:
            print(f"Command failed. Stderr:\n{e.stderr}", file=sys.stderr)
        if e.stdout:
            print(f"Command failed. Stdout:\n{e.stdout}", file=sys.stdout)
        raise e

def wait_for_crd(crd_name, timeout=30):
    print(f"Waiting for CRD {crd_name} to be established...")
    run_cmd(["kubectl", "wait", "--for=condition=Established", f"crd/{crd_name}", f"--timeout={timeout}s"])

def wait_for_webhook_ready():
    print("Waiting for conversion webhook to be responsive...")
    for i in range(30):
        res = subprocess.run(
            ["kubectl", "get", "sandboxwarmpools.extensions.agents.x-k8s.io", "--all-namespaces"],
            capture_output=True, text=True
        )
        if res.returncode == 0:
            print("Conversion webhook is responsive and ready!")
            return
        else:
            stderr = res.stderr.lower()
            if "conversion webhook" in stderr or "connection refused" in stderr or "webhook" in stderr:
                print(f"Webhook not ready yet (attempt {i+1}/30)...")
            else:
                print(f"List failed (attempt {i+1}/30): {res.stderr.strip()}")
            time.sleep(2)
    raise Exception("Timeout waiting for conversion webhook to become responsive")

def clear_kubectl_cache():
    import shutil
    cache_dir = os.path.expanduser("~/.kube/cache")
    http_cache_dir = os.path.expanduser("~/.kube/http-cache")
    print(f"Clearing kubectl cache: {cache_dir}, {http_cache_dir}")
    shutil.rmtree(cache_dir, ignore_errors=True)
    shutil.rmtree(http_cache_dir, ignore_errors=True)

def cleanup_sandbox_system():
    print("\n=== Phase 0: Cleaning up existing agent-sandbox installation ===")
    clear_kubectl_cache()
    
    crds = [
        "sandboxclaims.extensions.agents.x-k8s.io",
        "sandboxwarmpools.extensions.agents.x-k8s.io",
        "sandboxtemplates.extensions.agents.x-k8s.io",
        "sandboxes.agents.x-k8s.io"
    ]
    
    # 1. Delete objects
    for crd in crds:
        print(f"Deleting all resources of CRD {crd}...")
        run_cmd(["kubectl", "delete", crd, "--all", "--all-namespaces", "--ignore-not-found", "--timeout=30s"], check=False)
        
    print("Deleting agent-sandbox-system namespace...")
    run_cmd(["kubectl", "delete", "namespace", "agent-sandbox-system", "--ignore-not-found", "--timeout=60s"], check=False)
    
    for crd in crds:
        print(f"Deleting CRD {crd}...")
        run_cmd(["kubectl", "delete", "crd", crd, "--ignore-not-found", "--timeout=30s"], check=False)

    # Clean up RBAC and Webhooks
    run_cmd(["kubectl", "delete", "clusterrole", "agent-sandbox-controller", "--ignore-not-found"], check=False)
    run_cmd(["kubectl", "delete", "clusterrole", "agent-sandbox-controller-extensions", "--ignore-not-found"], check=False)
    run_cmd(["kubectl", "delete", "clusterrolebinding", "agent-sandbox-controller", "--ignore-not-found"], check=False)
    run_cmd(["kubectl", "delete", "clusterrolebinding", "agent-sandbox-controller-extensions", "--ignore-not-found"], check=False)
    run_cmd(["kubectl", "delete", "validatingwebhookconfiguration", "agent-sandbox-webhook", "--ignore-not-found"], check=False)
    run_cmd(["kubectl", "delete", "mutatingwebhookconfiguration", "agent-sandbox-webhook", "--ignore-not-found"], check=False)

def install_v1alpha1(method, version):
    print(f"\n=== Phase 1: Installing v1alpha1 version ({version}) using {method} ===")
    
    crds = [
        "sandboxclaims.extensions.agents.x-k8s.io",
        "sandboxwarmpools.extensions.agents.x-k8s.io",
        "sandboxtemplates.extensions.agents.x-k8s.io",
        "sandboxes.agents.x-k8s.io"
    ]

    if method == "kubectl":
        local_dir = os.path.join(_repo_root, "test/migration/testdata", version)
        local_manifest = os.path.join(local_dir, "manifest.yaml")
        local_extensions = os.path.join(local_dir, "extensions.yaml")
        
        if os.path.exists(local_manifest) and os.path.exists(local_extensions):
            print(f"Applying local manifest: {local_manifest}")
            run_cmd(["kubectl", "apply", "-f", local_manifest])
            print(f"Applying local extensions: {local_extensions}")
            run_cmd(["kubectl", "apply", "-f", local_extensions])
        else:
            manifest_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{version}/manifest.yaml"
            extensions_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{version}/extensions.yaml"
            print(f"Applying manifest: {manifest_url}")
            run_cmd(["kubectl", "apply", "-f", manifest_url])
            print(f"Applying extensions: {extensions_url}")
            run_cmd(["kubectl", "apply", "-f", extensions_url])
        
    elif method == "helm":
        # Strip leading 'v' for the helm package version if present
        helm_version = version[1:] if version.startswith("v") else version
        
        import tarfile
        import shutil
        
        temp_dir = os.path.join(_repo_root, "dev/tools/tmp_helm_chart")
        if os.path.exists(temp_dir):
            shutil.rmtree(temp_dir)
        os.makedirs(temp_dir)
        
        local_tarball = os.path.join(_repo_root, "test/migration/testdata", version, f"helm-{version}.tar.gz")
        
        try:
            if os.path.exists(local_tarball):
                print(f"Extracting local Helm chart from {local_tarball}...")
                with tarfile.open(local_tarball, "r:gz") as tar:
                    _safe_extract(tar, temp_dir)
                extracted_helm_path = os.path.join(temp_dir, "helm")
            else:
                import urllib.request
                tarball_url = f"https://github.com/kubernetes-sigs/agent-sandbox/archive/refs/tags/{version}.tar.gz"
                tarball_path = os.path.join(temp_dir, "archive.tar.gz")
                print(f"Downloading source archive from {tarball_url}...")
                urllib.request.urlretrieve(tarball_url, tarball_path)
                
                print("Extracting Helm chart...")
                with tarfile.open(tarball_path, "r:gz") as tar:
                    # Find the path to the helm directory in the archive
                    helm_src_dir = None
                    for member in tar.getmembers():
                        if (member.name.endswith("/helm") or member.name.endswith("/helm/")) and member.isdir():
                            helm_src_dir = member.name
                            break
                    if not helm_src_dir:
                        # Guess default structure
                        helm_src_dir = f"agent-sandbox-{helm_version}/helm/"
                    _safe_extract(tar, temp_dir)
                extracted_helm_path = os.path.join(temp_dir, helm_src_dir)
                
            print(f"Installing Helm release from extracted path: {extracted_helm_path}")
            run_cmd([
                "helm", "install", "agent-sandbox", extracted_helm_path,
                "-n", "agent-sandbox-system", "--create-namespace",
                "--set", "namespace.create=false",
                "--set", f"image.tag={version}",
                "--set", "controller.extensions=true"
            ])
        finally:
            # Clean up the downloaded and extracted files
            if os.path.exists(temp_dir):
                shutil.rmtree(temp_dir)

    
    # Wait for CRDs to be established before proceeding
    for crd in crds:
        wait_for_crd(crd)

    print("Waiting for agent-sandbox-controller deployment to be ready...")
    run_cmd(["kubectl", "rollout", "status", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=180s"])

def create_v1alpha1_objects():
    print("\n=== Phase 2: Creating v1alpha1 objects ===")
    run_cmd(["kubectl", "apply", "-f", "-"], input_data=V1ALPHA1_RESOURCES)
    
    # Explicitly patch the Sandbox with the owner reference pointing to upgrade-claim-warm
    res = run_cmd(["kubectl", "get", "sandboxclaim", "upgrade-claim-warm", "-n", "default", "-o", "jsonpath={.metadata.uid}"], capture_output=True)
    claim_uid = res.stdout.strip()
    owner_patch = {
        "metadata": {
            "ownerReferences": [
                {
                    "apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
                    "kind": "SandboxClaim",
                    "name": "upgrade-claim-warm",
                    "uid": claim_uid,
                    "controller": True,
                    "blockOwnerDeletion": True
                }
            ]
        }
    }
    run_cmd(["kubectl", "patch", "sandbox", "upgrade-pool-warm-a1b2c", "-n", "default", "--type=merge", "-p", json.dumps(owner_patch)])

    # Explicitly patch the status of upgrade-claim-warm since subresources.status drops status fields on normal apply
    run_cmd(["kubectl", "patch", "sandboxclaim", "upgrade-claim-warm", "-n", "default", "--subresource=status", "--type=merge", "-p", '{"status":{"sandbox":{"name":"upgrade-pool-warm-a1b2c"}}}'])

    
    print("Waiting for upgrade-sandbox-running Pod to be created...")
    pod_exists = False
    for i in range(60):
        res = run_cmd(["kubectl", "get", "pod", "upgrade-sandbox-running", "-n", "default"], check=False, capture_output=True)
        if res.returncode == 0:
            pod_exists = True
            break
        if (i + 1) % 5 == 0:
            print(f"Pod not created yet (attempt {i+1}/60)...")
        time.sleep(2)
        
    if not pod_exists:
        print("Pod upgrade-sandbox-running was not created in time. Dumping diagnostics:", file=sys.stderr)
        run_cmd(["kubectl", "get", "sandboxes,pods", "-n", "default"], check=False)
        run_cmd(["kubectl", "describe", "sandbox", "upgrade-sandbox-running", "-n", "default"], check=False)
        run_cmd(["kubectl", "logs", "-n", "agent-sandbox-system", "deploy/agent-sandbox-controller", "--tail=100"], check=False)
        
    assert pod_exists, "Pod upgrade-sandbox-running was not created in time!"
    
    print("Waiting for upgrade-sandbox-running Pod to be ready...")
    run_cmd([
        "kubectl", "wait", "--for=condition=Ready", "pod/upgrade-sandbox-running",
        "-n", "default", "--timeout=60s"
    ])
    
    # Get active pod details to verify disruption-free upgrade
    res = run_cmd(["kubectl", "get", "pod", "upgrade-sandbox-running", "-n", "default", "-o", "json"], capture_output=True)
    pod_data = json.loads(res.stdout)
    pod_uid = pod_data["metadata"]["uid"]
    pod_creation = pod_data["metadata"]["creationTimestamp"]
    print(f"Captured active pod info - Name: upgrade-sandbox-running, UID: {pod_uid}, CreatedAt: {pod_creation}")
    
    # Check claim exists and status has reconciled
    run_cmd(["kubectl", "get", "sandboxclaims.v1alpha1.extensions.agents.x-k8s.io", "-n", "default"])
    
    return {"uid": pod_uid, "creationTimestamp": pod_creation}

def upgrade_and_migrate(method, image_prefix, image_tag):
    print(f"\n=== Phase 3 & 4: Upgrading to target version & Migrating using {method} ===")
    
    # 1. Dry-run Bootstrap
    print("Running pre-upgrade migration bootstrap (dry-run)...")
    run_cmd(["bash", "dev/tools/migrate.sh", "--phase=bootstrap", "--dry-run"])
    # Verify shadow pool was NOT created
    res = run_cmd(["kubectl", "get", "sandboxwarmpools", "-n", "default", "-o", "json"], capture_output=True)
    pools = json.loads(res.stdout)["items"]
    for p in pools:
        assert p["metadata"].get("annotations", {}).get("agents.x-k8s.io/migration-shadow") != "true", "Dry-run bootstrap created a shadow pool!"
    print("Dry-run bootstrap validation PASSED.")

    # 2. Live Bootstrap
    print("Running pre-upgrade migration bootstrap...")
    run_cmd(["bash", "dev/tools/migrate.sh", "--phase=bootstrap"])
    
    # Verify shadow pool was created
    print("Verifying shadow pool creation...")
    res = run_cmd(["kubectl", "get", "sandboxwarmpool", "shadow-pool-upgrade-template-cold", "-n", "default", "-o", "json"], capture_output=True)
    shadow_pool = json.loads(res.stdout)
    assert shadow_pool["spec"]["sandboxTemplateRef"]["name"] == "upgrade-template-cold", "Shadow pool template mismatch!"
    print("Shadow pool successfully verified!")

    # 3. Upgrade Controller & CRDs
    if method == "kubectl":
        print("Deploying target controller/CRDs...")
        deploy_cmd = ["python3", "dev/tools/deploy-to-kube", "--extensions"]
        if image_prefix:
            deploy_cmd.extend(["--image-prefix", image_prefix])
        if image_tag:
            deploy_cmd.extend(["--image-tag", image_tag])
        run_cmd(deploy_cmd)
        
    elif method == "helm":
        print("Applying upgraded CRD manifests using Server-Side Apply...")
        run_cmd(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", "./helm/crds/"])
        
        print("Upgrading via local Helm chart...")
        upgrade_cmd = [
            "helm", "upgrade", "agent-sandbox", "./helm/",
            "-n", "agent-sandbox-system",
            "--set", "namespace.create=false",
            "--set", "controller.extensions=true",
            "--set", "webhookServiceName=custom-webhook-svc"
        ]
        if image_prefix:
            repo = f"{image_prefix}agent-sandbox-controller"
            upgrade_cmd.extend(["--set", f"image.repository={repo}"])
        if image_tag:
            upgrade_cmd.extend(["--set", f"image.tag={image_tag}"])
        run_cmd(upgrade_cmd)

    print("Waiting for upgraded controller deployment...")
    run_cmd(["kubectl", "rollout", "status", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=180s"])
    
    if method == "helm":
        res = run_cmd(["kubectl", "get", "deployment", "agent-sandbox-controller", "-n", "agent-sandbox-system", "-o", "jsonpath={.spec.template.spec.containers[0].args}"], capture_output=True)
        args_str = res.stdout
        assert "--webhook-service-name=custom-webhook-svc" in args_str, f"Custom webhook service name flag not wired! Got args: {args_str}"
        assert "--webhook-namespace=agent-sandbox-system" in args_str, f"Webhook namespace flag not wired! Got args: {args_str}"
        print("Helm custom webhook CLI flags wiring validation PASSED.")
    
    wait_for_webhook_ready()

    # 4. Dry-run Migrate
    print("Running post-upgrade storage rewrite (dry-run migrate)...")
    run_cmd(["bash", "dev/tools/migrate.sh", "--phase=migrate", "--dry-run"])
    # Verify no resources have the storage-migrated-at annotation
    for resource_type in ["sandboxes", "sandboxclaims", "sandboxtemplates", "sandboxwarmpools"]:
        res = run_cmd(["kubectl", "get", resource_type, "-n", "default", "-o", "json"], capture_output=True)
        items = json.loads(res.stdout)["items"]
        for item in items:
            assert "agents.x-k8s.io/storage-migrated-at" not in item.get("metadata", {}).get("annotations", {}), f"Dry-run migrate modified resource {item['metadata']['name']}!"
    print("Dry-run migrate validation PASSED.")

    # 5. Live Migrate
    print("Running post-upgrade storage rewrite (migrate phase)...")
    run_cmd(["bash", "dev/tools/migrate.sh", "--phase=migrate"])


def validate_migration(active_pod_info):
    print("\n=== Validation Phase: Asserting converted objects ===")
    
    # 1. Fetch claims as JSON
    print("Checking SandboxClaims...")
    res = run_cmd(["kubectl", "get", "sandboxclaims.v1beta1.extensions.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    claims = json.loads(res.stdout)["items"]
    
    claim_by_name = {c["metadata"]["name"]: c for c in claims}
    
    # Validate upgrade-claim: cold-start, pointed to "default" warmpool in v1alpha1,
    # should be migrated to shadow-pool-upgrade-template.
    assert "upgrade-claim" in claim_by_name, "upgrade-claim missing!"
    claim1 = claim_by_name["upgrade-claim"]
    
    print("Validating upgrade-claim conversion...")
    assert "warmPoolRef" in claim1["spec"], f"upgrade-claim missing warmPoolRef! spec: {claim1['spec']}"
    assert claim1["spec"]["warmPoolRef"]["name"] == "shadow-pool-upgrade-template-cold", \
        f"Expected warmPoolRef name shadow-pool-upgrade-template-cold, got {claim1['spec']['warmPoolRef']['name']}"
    assert "agents.x-k8s.io/storage-migrated-at" in claim1["metadata"]["annotations"], \
        "upgrade-claim missing storage-migrated-at annotation!"
    print("upgrade-claim validation PASSED.")
    
    # Validate upgrade-claim-specific: pointed to "upgrade-pool" in v1alpha1,
    # should keep "upgrade-pool" verbatim in warmPoolRef.name.
    assert "upgrade-claim-specific" in claim_by_name, "upgrade-claim-specific missing!"
    claim2 = claim_by_name["upgrade-claim-specific"]
    
    print("Validating upgrade-claim-specific conversion...")
    assert "warmPoolRef" in claim2["spec"], f"upgrade-claim-specific missing warmPoolRef! spec: {claim2['spec']}"
    assert claim2["spec"]["warmPoolRef"]["name"] == "upgrade-pool", \
        f"Expected warmPoolRef name upgrade-pool, got {claim2['spec']['warmPoolRef']['name']}"
    assert "agents.x-k8s.io/storage-migrated-at" in claim2["metadata"]["annotations"], \
        "upgrade-claim-specific missing storage-migrated-at annotation!"
    print("upgrade-claim-specific validation PASSED.")

    # Validate upgrade-claim-none: cold-start, pointed to "none" in v1alpha1,
    # should be migrated to shadow-pool-upgrade-template.
    assert "upgrade-claim-none" in claim_by_name, "upgrade-claim-none missing!"
    claim_none = claim_by_name["upgrade-claim-none"]
    print("Validating upgrade-claim-none conversion...")
    assert "warmPoolRef" in claim_none["spec"], f"upgrade-claim-none missing warmPoolRef! spec: {claim_none['spec']}"
    assert claim_none["spec"]["warmPoolRef"]["name"] == "shadow-pool-upgrade-template-cold", \
        f"Expected warmPoolRef name shadow-pool-upgrade-template-cold, got {claim_none['spec']['warmPoolRef']['name']}"
    assert "agents.x-k8s.io/storage-migrated-at" in claim_none["metadata"]["annotations"], \
        "upgrade-claim-none missing storage-migrated-at annotation!"
    print("upgrade-claim-none validation PASSED.")
    
    # Validate upgrade-claim-warm: warm-started, pointed to "default" in v1alpha1 with bound sandbox upgrade-pool-warm-a1b2c,
    # should be migrated to warmPoolRef.name: upgrade-pool-warm (derived from stripping suffix).
    assert "upgrade-claim-warm" in claim_by_name, "upgrade-claim-warm missing!"
    claim_warm = claim_by_name["upgrade-claim-warm"]
    print("Validating upgrade-claim-warm conversion...")
    assert "warmPoolRef" in claim_warm["spec"], f"upgrade-claim-warm missing warmPoolRef! spec: {claim_warm['spec']}"
    assert claim_warm["spec"]["warmPoolRef"]["name"] == "upgrade-pool-warm", \
        f"Expected warmPoolRef name upgrade-pool-warm, got {claim_warm['spec']['warmPoolRef']['name']}"
    assert "agents.x-k8s.io/storage-migrated-at" in claim_warm["metadata"]["annotations"], \
        "upgrade-claim-warm missing storage-migrated-at annotation!"
    print("upgrade-claim-warm validation PASSED.")
    
    # 2. Fetch sandboxes as JSON
    print("Checking Sandboxes...")
    res = run_cmd(["kubectl", "get", "sandboxes.v1beta1.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    sandboxes = json.loads(res.stdout)["items"]
    sandbox_by_name = {s["metadata"]["name"]: s for s in sandboxes}

    # Validate upgrade-sandbox: had replicas: 0 in v1alpha1, operatingMode should be Suspended.
    assert "upgrade-sandbox" in sandbox_by_name, "upgrade-sandbox missing!"
    sb = sandbox_by_name["upgrade-sandbox"]
    
    print("Validating upgrade-sandbox conversion...")
    assert "operatingMode" in sb["spec"], f"upgrade-sandbox missing operatingMode! spec: {sb['spec']}"
    assert sb["spec"]["operatingMode"] == "Suspended", \
        f"Expected operatingMode Suspended, got {sb['spec']['operatingMode']}"
    assert "agents.x-k8s.io/storage-migrated-at" in sb["metadata"]["annotations"], \
        "upgrade-sandbox missing storage-migrated-at annotation!"
    print("upgrade-sandbox validation PASSED.")

    # Validate upgrade-sandbox-running: had replicas: 1 in v1alpha1, operatingMode should be Running.
    assert "upgrade-sandbox-running" in sandbox_by_name, "upgrade-sandbox-running missing!"
    sb_running = sandbox_by_name["upgrade-sandbox-running"]
    
    print("Validating upgrade-sandbox-running conversion...")
    assert "operatingMode" in sb_running["spec"], f"upgrade-sandbox-running missing operatingMode! spec: {sb_running['spec']}"
    assert sb_running["spec"]["operatingMode"] == "Running", \
        f"Expected operatingMode Running, got {sb_running['spec']['operatingMode']}"
    assert "agents.x-k8s.io/storage-migrated-at" in sb_running["metadata"]["annotations"], \
        "upgrade-sandbox-running missing storage-migrated-at annotation!"
    print("upgrade-sandbox-running validation PASSED.")

    # Verify that the underlying Pod was NOT disrupted (recreated/restarted)
    print("Validating upgrade-sandbox-running Pod stability...")
    res = run_cmd(["kubectl", "get", "pod", "upgrade-sandbox-running", "-n", "default", "-o", "json"], capture_output=True)
    pod_data = json.loads(res.stdout)
    assert pod_data["metadata"]["uid"] == active_pod_info["uid"], "Pod UID changed! The running Pod was recreated/disrupted during conversion."
    assert pod_data["metadata"]["creationTimestamp"] == active_pod_info["creationTimestamp"], "Pod creationTimestamp changed! The running Pod was recreated/disrupted during conversion."
    print("upgrade-sandbox-running Pod disruption validation PASSED (Pod UID and creationTimestamp unchanged).")
    
    # 2.5 Verify reverse conversion (v1beta1 -> v1alpha1) via the webhook
    print("\n=== Validation Phase: Asserting dynamic v1beta1 -> v1alpha1 conversion ===")
    
    # Query SandboxClaims using v1alpha1 API version
    print("Checking SandboxClaims via v1alpha1 endpoint...")
    res = run_cmd(["kubectl", "get", "sandboxclaims.v1alpha1.extensions.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    claims_v1alpha1 = json.loads(res.stdout)["items"]
    claims_v1alpha1_by_name = {c["metadata"]["name"]: c for c in claims_v1alpha1}
    
    # Validate upgrade-claim: spec should be dynamically converted back to warmpool: "default"
    assert "upgrade-claim" in claims_v1alpha1_by_name, "upgrade-claim missing in v1alpha1 list!"
    c1_v1alpha1 = claims_v1alpha1_by_name["upgrade-claim"]
    assert "warmpool" in c1_v1alpha1["spec"], f"upgrade-claim missing warmpool in v1alpha1 spec! spec: {c1_v1alpha1['spec']}"
    assert c1_v1alpha1["spec"]["warmpool"] == "default", f"Expected warmpool default, got {c1_v1alpha1['spec']['warmpool']}"
    assert "warmPoolRef" not in c1_v1alpha1["spec"], f"upgrade-claim should NOT have warmPoolRef in v1alpha1 spec! spec: {c1_v1alpha1['spec']}"
    print("upgrade-claim dynamic reverse-conversion validation PASSED.")
    
    # Validate upgrade-claim-none: spec should be dynamically converted back to warmpool: "none"
    assert "upgrade-claim-none" in claims_v1alpha1_by_name, "upgrade-claim-none missing in v1alpha1 list!"
    c_none_v1alpha1 = claims_v1alpha1_by_name["upgrade-claim-none"]
    assert "warmpool" in c_none_v1alpha1["spec"], f"upgrade-claim-none missing warmpool in v1alpha1 spec! spec: {c_none_v1alpha1['spec']}"
    assert c_none_v1alpha1["spec"]["warmpool"] == "none", f"Expected warmpool none, got {c_none_v1alpha1['spec']['warmpool']}"
    assert "warmPoolRef" not in c_none_v1alpha1["spec"], f"upgrade-claim-none should NOT have warmPoolRef in v1alpha1 spec! spec: {c_none_v1alpha1['spec']}"
    print("upgrade-claim-none dynamic reverse-conversion validation PASSED.")
    
    # Validate upgrade-claim-warm: spec should be dynamically converted back to warmpool: "default"
    assert "upgrade-claim-warm" in claims_v1alpha1_by_name, "upgrade-claim-warm missing in v1alpha1 list!"
    c_warm_v1alpha1 = claims_v1alpha1_by_name["upgrade-claim-warm"]
    assert "warmpool" in c_warm_v1alpha1["spec"], f"upgrade-claim-warm missing warmpool in v1alpha1 spec! spec: {c_warm_v1alpha1['spec']}"
    assert c_warm_v1alpha1["spec"]["warmpool"] == "default", f"Expected warmpool default, got {c_warm_v1alpha1['spec']['warmpool']}"
    assert "warmPoolRef" not in c_warm_v1alpha1["spec"], f"upgrade-claim-warm should NOT have warmPoolRef in v1alpha1 spec! spec: {c_warm_v1alpha1['spec']}"
    print("upgrade-claim-warm dynamic reverse-conversion validation PASSED.")
    
    # Query Sandboxes using v1alpha1 API version
    print("Checking Sandboxes via v1alpha1 endpoint...")
    res = run_cmd(["kubectl", "get", "sandboxes.v1alpha1.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    sandboxes_v1alpha1 = json.loads(res.stdout)["items"]
    sandboxes_v1alpha1_by_name = {s["metadata"]["name"]: s for s in sandboxes_v1alpha1}
    
    # Validate upgrade-sandbox: operatingMode: Suspended should be dynamically converted back to replicas: 0
    assert "upgrade-sandbox" in sandboxes_v1alpha1_by_name, "upgrade-sandbox missing in v1alpha1 list!"
    sb_v1alpha1 = sandboxes_v1alpha1_by_name["upgrade-sandbox"]
    assert "replicas" in sb_v1alpha1["spec"], f"upgrade-sandbox missing replicas in v1alpha1 spec! spec: {sb_v1alpha1['spec']}"
    assert sb_v1alpha1["spec"]["replicas"] == 0, f"Expected replicas 0, got {sb_v1alpha1['spec']['replicas']}"
    assert "operatingMode" not in sb_v1alpha1["spec"], f"upgrade-sandbox should NOT have operatingMode in v1alpha1 spec! spec: {sb_v1alpha1['spec']}"
    print("upgrade-sandbox dynamic reverse-conversion validation PASSED.")
    
    # Validate upgrade-sandbox-running: operatingMode: Running should be dynamically converted back to replicas: 1
    assert "upgrade-sandbox-running" in sandboxes_v1alpha1_by_name, "upgrade-sandbox-running missing in v1alpha1 list!"
    sb_running_v1alpha1 = sandboxes_v1alpha1_by_name["upgrade-sandbox-running"]
    assert "replicas" in sb_running_v1alpha1["spec"], f"upgrade-sandbox-running missing replicas in v1alpha1 spec! spec: {sb_running_v1alpha1['spec']}"
    assert sb_running_v1alpha1["spec"]["replicas"] == 1, f"Expected replicas 1, got {sb_running_v1alpha1['spec']['replicas']}"
    assert "operatingMode" not in sb_running_v1alpha1["spec"], f"upgrade-sandbox-running should NOT have operatingMode in v1alpha1 spec! spec: {sb_running_v1alpha1['spec']}"
    print("upgrade-sandbox-running dynamic reverse-conversion validation PASSED.")
    
    # 3. Clean up storedVersions in CRDs
    print("Pruning v1alpha1 from CRD storedVersions...")
    crds = [
        "sandboxes.agents.x-k8s.io",
        "sandboxclaims.extensions.agents.x-k8s.io",
        "sandboxtemplates.extensions.agents.x-k8s.io",
        "sandboxwarmpools.extensions.agents.x-k8s.io"
    ]
    for crd in crds:
        print(f"Pruning storedVersions for {crd}...")
        patch = '{"status":{"storedVersions":["v1beta1"]}}'
        run_cmd(["kubectl", "patch", "crd", crd, "--subresource=status", "--type=merge", "-p", patch])
        
        # Verify the storedVersions has been successfully pruned to just v1beta1
        res = run_cmd(["kubectl", "get", "crd", crd, "-o", "jsonpath={.status.storedVersions}"], capture_output=True)
        try:
            stored_versions = json.loads(res.stdout)
        except json.JSONDecodeError as e:
            raise AssertionError(f"Failed to parse storedVersions for CRD {crd}. Output: {res.stdout}. Error: {e}") from e
        assert stored_versions == ["v1beta1"], f"CRD {crd} storedVersions not pruned! Got {stored_versions}"
        print(f"CRD {crd} storedVersions successfully pruned: {stored_versions}")
        
    print("\nALL MIGRATION TESTS PASSED SUCCESSFULLY!")

def test_rollback(method, v1alpha1_version, v1alpha1_backup):
    print("\n=== Rollback Phase: Reverting to v1alpha1 ===")
    
    crds = [
        "sandboxclaims.extensions.agents.x-k8s.io",
        "sandboxwarmpools.extensions.agents.x-k8s.io",
        "sandboxtemplates.extensions.agents.x-k8s.io",
        "sandboxes.agents.x-k8s.io"
    ]
    
    # Step 1: Disable conversion webhook 
    print("Step 1: Disabling conversion webhooks...")
    for crd in crds:
        patch = '{"spec":{"conversion":{"strategy":"None","webhook":null}}}'
        run_cmd(["kubectl", "patch", "crd", crd, "--type=merge", "-p", patch])

    # Scale down the controller deployment to 0 replicas to prevent race conditions during deletion
    print("Scaling down controller deployment to 0 replicas...")
    run_cmd(["kubectl", "scale", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--replicas=0"])
    
    # Wait for the controller pods to terminate completely
    print("Waiting for controller pods to terminate...")
    run_cmd(["kubectl", "wait", "--for=delete", "pod", "-l", "app=agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=60s"], check=False)

    # Step 2: Delete upgraded resources while upgraded CRD spec (v1beta1) is still installed
    print("Step 2: Deleting upgraded resources from etcd...")
    run_cmd(["kubectl", "delete", "sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools", "-A", "--all"])

    # Step 3: Delete shadow warm pools
    print("Step 3: Deleting shadow warm pools using jq pipeline...")
    cmd = (
        "kubectl get sandboxwarmpools -A -o json | "
        "jq -r '.items[] | select(.metadata.annotations[\"agents.x-k8s.io/migration-shadow\"]==\"true\") | \"\\(.metadata.namespace)/\\(.metadata.name)\"' | "
        "xargs -I {} sh -c 'kubectl delete sandboxwarmpool $(echo {} | cut -d/ -f2) -n $(echo {} | cut -d/ -f1)'"
    )
    run_cmd(["bash", "-c", cmd])

    # Step 4: Reset storedVersions to v1alpha1 in CRD status
    print("Step 4: Resetting CRD status.storedVersions to ['v1alpha1']...")
    for crd in crds:
        patch = '{"status":{"storedVersions":["v1alpha1"]}}'
        run_cmd(["kubectl", "patch", "crd", crd, "--subresource=status", "--type=merge", "-p", patch])
        
    # Step 5: Downgrade controller and CRDs to v1alpha1
    print("Step 5: Downgrading controller and CRDs to v1alpha1...")
    if method == "kubectl":
        local_dir = os.path.join(_repo_root, "test/migration/testdata", v1alpha1_version)
        local_manifest = os.path.join(local_dir, "manifest.yaml")
        local_extensions = os.path.join(local_dir, "extensions.yaml")
        
        if os.path.exists(local_manifest) and os.path.exists(local_extensions):
            print(f"Applying local manifest: {local_manifest}")
            run_cmd(["kubectl", "apply", "-f", local_manifest])
            print(f"Applying local extensions: {local_extensions}")
            run_cmd(["kubectl", "apply", "-f", local_extensions])
        else:
            manifest_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{v1alpha1_version}/manifest.yaml"
            extensions_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{v1alpha1_version}/extensions.yaml"
            run_cmd(["kubectl", "apply", "-f", manifest_url])
            run_cmd(["kubectl", "apply", "-f", extensions_url])
            
    elif method == "helm":
        res = run_cmd(["helm", "history", "agent-sandbox", "-n", "agent-sandbox-system", "-o", "json"], capture_output=True)
        history = json.loads(res.stdout)
        prev_revision = 1
        for h in history:
            if h["description"] == "Install complete":
                prev_revision = h["revision"]
                break
        print(f"Rolling back Helm release to revision {prev_revision}...")
        run_cmd(["helm", "rollback", "agent-sandbox", str(prev_revision), "-n", "agent-sandbox-system"])
        
        local_tarball = os.path.join(_repo_root, "test/migration/testdata", v1alpha1_version, f"helm-{v1alpha1_version}.tar.gz")
        
        import tarfile
        import shutil
        
        temp_dir = os.path.join(_repo_root, "dev/tools/tmp_rollback_crds")
        shutil.rmtree(temp_dir, ignore_errors=True)
        os.makedirs(temp_dir)
        
        try:
            if os.path.exists(local_tarball):
                print(f"Extracting local Helm CRDs from {local_tarball}...")
                with tarfile.open(local_tarball, "r:gz") as tar:
                    _safe_extract(tar, temp_dir)
                extracted_crds_path = os.path.join(temp_dir, "helm/crds")
            else:
                # Download old source archive to extract and manually downgrade the CRDs
                print(f"Downloading old source archive for version {v1alpha1_version} to extract and manually downgrade CRDs...")
                import urllib.request
                tarball_url = f"https://github.com/kubernetes-sigs/agent-sandbox/archive/refs/tags/{v1alpha1_version}.tar.gz"
                tarball_path = os.path.join(temp_dir, "archive.tar.gz")
                urllib.request.urlretrieve(tarball_url, tarball_path)
                
                with tarfile.open(tarball_path, "r:gz") as tar:
                    crds_src_dir = None
                    for member in tar.getmembers():
                        if member.name.endswith("/helm/crds") or member.name.endswith("/helm/crds/"):
                            crds_src_dir = member.name
                            break
                    if not crds_src_dir:
                        for member in tar.getmembers():
                            if member.name.endswith("/crds") or member.name.endswith("/crds/"):
                                crds_src_dir = member.name
                                break
                    assert crds_src_dir, f"Could not find helm/crds directory in source archive of {v1alpha1_version}!"
                    members = [m for m in tar.getmembers() if m.name.startswith(crds_src_dir)]
                    _safe_extract(tar, temp_dir, members=members)
                extracted_crds_path = os.path.join(temp_dir, crds_src_dir)
                
            # Apply the old CRDs using server-side apply
            run_cmd(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", extracted_crds_path])
        finally:
            shutil.rmtree(temp_dir, ignore_errors=True)

    # Wait for the CRDs to be re-established under v1alpha1
    for crd in crds:
        wait_for_crd(crd)
        
    clear_kubectl_cache()
    
    # Sleep a bit to let the API server completely re-initialize the storage handlers for the new schemas
    print("Waiting for API server storage to re-initialize...")
    time.sleep(5)
        
    print("Waiting for downgraded controller deployment...")
    run_cmd(["kubectl", "rollout", "status", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=180s"])
    
    # Step 6: Restore data from backup
    print("Step 6: Applying cleaned v1alpha1 backup...")
    # Clean status and metadata fields from backup
    import yaml
    backup_data = list(yaml.safe_load_all(v1alpha1_backup))
    cleaned_items = []
    for doc in backup_data:
        if not doc:
            continue
        if doc.get("kind") == "List":
            for item in doc.get("items", []):
                cleaned_items.append(item)
        else:
            cleaned_items.append(doc)
            
    for item in cleaned_items:
        item.pop("status", None)
        meta = item.get("metadata", {})
        meta.pop("resourceVersion", None)
        meta.pop("uid", None)
        meta.pop("creationTimestamp", None)
        meta.pop("generation", None)
        meta.pop("selfLink", None)
        meta.pop("ownerReferences", None)
        meta.pop("managedFields", None)
        
    cleaned_yaml = yaml.dump_all(cleaned_items)
    run_cmd(["kubectl", "apply", "-f", "-"], input_data=cleaned_yaml)

def validate_rollback():
    print("\n=== Validation Phase: Asserting rolled-back objects ===")
    
    # 1. Fetch claims as JSON
    print("Checking SandboxClaims...")
    res = run_cmd(["kubectl", "get", "sandboxclaims.v1alpha1.extensions.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    claims = json.loads(res.stdout)["items"]
    claim_by_name = {c["metadata"]["name"]: c for c in claims}
    
    # Validate upgrade-claim
    assert "upgrade-claim" in claim_by_name, "upgrade-claim missing!"
    claim1 = claim_by_name["upgrade-claim"]
    assert "warmpool" in claim1["spec"], f"upgrade-claim missing warmpool policy! spec: {claim1['spec']}"
    assert claim1["spec"]["warmpool"] == "default", f"Expected warmpool default, got {claim1['spec']['warmpool']}"
    assert "warmPoolRef" not in claim1["spec"], f"upgrade-claim should NOT have warmPoolRef! spec: {claim1['spec']}"
    print("upgrade-claim rollback validation PASSED.")
    
    # Validate upgrade-claim-specific
    assert "upgrade-claim-specific" in claim_by_name, "upgrade-claim-specific missing!"
    claim2 = claim_by_name["upgrade-claim-specific"]
    assert "warmpool" in claim2["spec"], f"upgrade-claim-specific missing warmpool policy! spec: {claim2['spec']}"
    assert claim2["spec"]["warmpool"] == "upgrade-pool", f"Expected warmpool upgrade-pool, got {claim2['spec']['warmpool']}"
    assert "warmPoolRef" not in claim2["spec"], f"upgrade-claim-specific should NOT have warmPoolRef! spec: {claim2['spec']}"
    print("upgrade-claim-specific rollback validation PASSED.")
    
    # Validate upgrade-claim-none
    assert "upgrade-claim-none" in claim_by_name, "upgrade-claim-none missing!"
    claim_none = claim_by_name["upgrade-claim-none"]
    assert "warmpool" in claim_none["spec"], f"upgrade-claim-none missing warmpool policy! spec: {claim_none['spec']}"
    assert claim_none["spec"]["warmpool"] == "none", f"Expected warmpool none, got {claim_none['spec']['warmpool']}"
    assert "warmPoolRef" not in claim_none["spec"], f"upgrade-claim-none should NOT have warmPoolRef! spec: {claim_none['spec']}"
    print("upgrade-claim-none rollback validation PASSED.")
    
    # Validate upgrade-claim-warm
    assert "upgrade-claim-warm" in claim_by_name, "upgrade-claim-warm missing!"
    claim_warm = claim_by_name["upgrade-claim-warm"]
    assert "warmpool" in claim_warm["spec"], f"upgrade-claim-warm missing warmpool policy! spec: {claim_warm['spec']}"
    assert claim_warm["spec"]["warmpool"] == "default", f"Expected warmpool default, got {claim_warm['spec']['warmpool']}"
    assert "warmPoolRef" not in claim_warm["spec"], f"upgrade-claim-warm should NOT have warmPoolRef! spec: {claim_warm['spec']}"
    print("upgrade-claim-warm rollback validation PASSED.")
    
    # 2. Fetch sandboxes as JSON
    print("Checking Sandboxes...")
    res = run_cmd(["kubectl", "get", "sandboxes.v1alpha1.agents.x-k8s.io", "-n", "default", "-o", "json"], capture_output=True)
    sandboxes = json.loads(res.stdout)["items"]
    sandbox_by_name = {s["metadata"]["name"]: s for s in sandboxes}
    
    # Validate upgrade-sandbox
    assert "upgrade-sandbox" in sandbox_by_name, "upgrade-sandbox missing!"
    sb = sandbox_by_name["upgrade-sandbox"]
    assert "replicas" in sb["spec"], f"upgrade-sandbox missing replicas field! spec: {sb['spec']}"
    assert sb["spec"]["replicas"] == 0, f"Expected replicas 0, got {sb['spec']['replicas']}"
    assert "operatingMode" not in sb["spec"], f"upgrade-sandbox should NOT have operatingMode! spec: {sb['spec']}"
    print("upgrade-sandbox rollback validation PASSED.")

    # Validate upgrade-sandbox-running
    assert "upgrade-sandbox-running" in sandbox_by_name, "upgrade-sandbox-running missing!"
    sb_running = sandbox_by_name["upgrade-sandbox-running"]
    assert "replicas" in sb_running["spec"], f"upgrade-sandbox-running missing replicas field! spec: {sb_running['spec']}"
    assert sb_running["spec"]["replicas"] == 1, f"Expected replicas 1, got {sb_running['spec']['replicas']}"
    assert "operatingMode" not in sb_running["spec"], f"upgrade-sandbox-running should NOT have operatingMode! spec: {sb_running['spec']}"
    print("upgrade-sandbox-running rollback validation PASSED.")
    
    print("\nALL ROLLBACK VALIDATIONS PASSED SUCCESSFULLY!")

def main():
    parser = argparse.ArgumentParser(description="Run E2E migration tests for agent-sandbox")
    parser.add_argument("--image-prefix",
                        dest="image_prefix",
                        help="registry/prefix for target images. Defaults to kind.local/",
                        type=str,
                        default="kind.local/")
    parser.add_argument("--image-tag",
                        dest="image_tag",
                        help="tag for target images. Defaults to local built tag",
                        type=str,
                        default=None)
    parser.add_argument("--method",
                        dest="method",
                        choices=["kubectl", "helm"],
                        help="Upgrade method to use (kubectl or helm). Default is kubectl",
                        type=str,
                        default="kubectl")
    parser.add_argument("--v1alpha1-version",
                        dest="v1alpha1_version",
                        help="The old version to install (e.g. v0.4.6). Default is v0.4.6",
                        type=str,
                        default="v0.4.6")
    parser.add_argument("--keep-resources",
                        dest="keep_resources",
                        action="store_true",
                        help="Keep the resources and controller namespace after validation for debugging.")
    parser.add_argument("--test-rollback",
                        dest="test_rollback",
                        action="store_true",
                        help="Run a rollback test and validate that resources revert to v1alpha1.")
    
    args = parser.parse_args()
    
    # 0. Clean up existing sandbox
    cleanup_sandbox_system()
    
    try:
        # 1. Install old v1alpha1 version
        install_v1alpha1(args.method, args.v1alpha1_version)
        
        # 2. Create v1alpha1 CR instances
        active_pod_info = create_v1alpha1_objects()
        
        # Backup v1alpha1 resources in memory
        print("Backing up v1alpha1 resources...")
        res = run_cmd([
            "kubectl", "get", "sandboxes,sandboxclaims,sandboxtemplates,sandboxwarmpools",
            "-n", "default", "-o", "yaml"
        ], capture_output=True)
        v1alpha1_backup = res.stdout
        
        # 3. Upgrade and run migration
        upgrade_and_migrate(args.method, args.image_prefix, args.image_tag)
        
        # 4. Perform final validation
        validate_migration(active_pod_info)
        
        # 5. Optionally run and validate rollback
        if args.test_rollback:
            test_rollback(args.method, args.v1alpha1_version, v1alpha1_backup)
            validate_rollback()
            
    finally:
        if not args.keep_resources:
            print("\nCleaning up resources...")
            cleanup_sandbox_system()
        else:
            print("\nResources kept as requested for debugging.")

if __name__ == "__main__":
    main()
