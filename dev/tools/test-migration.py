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
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: upgrade-claim
  namespace: default
spec:
  sandboxTemplateRef:
    name: upgrade-template
  warmpool: "default" # v1alpha1 syntax (converts to warmPoolRef.name: shadow-pool-upgrade-template)
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
"""

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
        manifest_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{version}/manifest.yaml"
        extensions_url = f"https://github.com/kubernetes-sigs/agent-sandbox/releases/download/{version}/extensions.yaml"
        
        print(f"Applying manifest: {manifest_url}")
        run_cmd(["kubectl", "apply", "-f", manifest_url])
        
        print(f"Applying extensions: {extensions_url}")
        run_cmd(["kubectl", "apply", "-f", extensions_url])
        
    elif method == "helm":
        # Strip leading 'v' for the helm package version if present
        helm_version = version[1:] if version.startswith("v") else version
        
        # We download the source tarball from GitHub and extract the helm chart subdirectory
        import urllib.request
        import tarfile
        import shutil
        
        temp_dir = os.path.join(_repo_root, "dev/tools/tmp_helm_chart")
        if os.path.exists(temp_dir):
            shutil.rmtree(temp_dir)
        os.makedirs(temp_dir)
        
        tarball_url = f"https://github.com/kubernetes-sigs/agent-sandbox/archive/refs/tags/{version}.tar.gz"
        tarball_path = os.path.join(temp_dir, "archive.tar.gz")
        
        print(f"Downloading source archive from {tarball_url}...")
        try:
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
                tar.extractall(path=temp_dir)
                
            extracted_helm_path = os.path.join(temp_dir, helm_src_dir)
            print(f"Installing Helm release from extracted path: {extracted_helm_path}")
            
            run_cmd([
                "helm", "install", "agent-sandbox", extracted_helm_path,
                "-n", "agent-sandbox-system", "--create-namespace",
                "--set", "namespace.create=false",
                "--set", f"image.tag={version}"
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
    
    print("Waiting for v1alpha1 claims to be bound...")
    # Give a short sleep for claims to reconcile
    time.sleep(10)
    # Check claim exists and status has reconciled
    run_cmd(["kubectl", "get", "sandboxclaims.v1alpha1.extensions.agents.x-k8s.io", "-n", "default"])

def upgrade_and_migrate(method, image_prefix, image_tag):
    print(f"\n=== Phase 3 & 4: Upgrading to target version & Migrating using {method} ===")
    
    if method == "kubectl":
        print("Running pre-upgrade migration bootstrap...")
        run_cmd(["bash", "dev/tools/migrate.sh", "--phase=bootstrap"])
        
        # Verify shadow pool was created
        print("Verifying shadow pool creation...")
        res = run_cmd(["kubectl", "get", "sandboxwarmpool", "shadow-pool-upgrade-template", "-n", "default", "-o", "json"], capture_output=True)
        shadow_pool = json.loads(res.stdout)
        assert shadow_pool["spec"]["sandboxTemplateRef"]["name"] == "upgrade-template", "Shadow pool template mismatch!"
        print("Shadow pool successfully verified!")
        
        # Run local deploy command to upgrade controller to new v1beta1 version
        print("Deploying target controller/CRDs...")
        deploy_cmd = ["python3", "dev/tools/deploy-to-kube", "--extensions"]
        if image_prefix:
            deploy_cmd.extend(["--image-prefix", image_prefix])
        if image_tag:
            deploy_cmd.extend(["--image-tag", image_tag])
            
        run_cmd(deploy_cmd)
        
        print("Waiting for upgraded controller deployment...")
        run_cmd(["kubectl", "rollout", "status", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=180s"])
        
        wait_for_webhook_ready()
        
        print("Running post-upgrade storage rewrite (migrate phase)...")
        run_cmd(["bash", "dev/tools/migrate.sh", "--phase=migrate"])
        
    elif method == "helm":
        print("Running pre-upgrade bootstrap phase...")
        run_cmd(["bash", "dev/tools/migrate.sh", "--phase=bootstrap"])
        
        print("Applying upgraded CRD manifests using Server-Side Apply...")
        run_cmd(["kubectl", "apply", "--server-side", "--force-conflicts", "-f", "./helm/crds/"])
        
        print("Upgrading via local Helm chart...")
        upgrade_cmd = [
            "helm", "upgrade", "agent-sandbox", "./helm/",
            "-n", "agent-sandbox-system",
            "--set", "namespace.create=false",
            "--set", "controller.extensions=true"
        ]
        if image_prefix:
            repo = f"{image_prefix}agent-sandbox-controller"
            upgrade_cmd.extend(["--set", f"image.repository={repo}"])
        if image_tag:
            upgrade_cmd.extend(["--set", f"image.tag={image_tag}"])
            
        run_cmd(upgrade_cmd)
        
        print("Waiting for upgraded controller deployment...")
        run_cmd(["kubectl", "rollout", "status", "deploy/agent-sandbox-controller", "-n", "agent-sandbox-system", "--timeout=180s"])
        
        wait_for_webhook_ready()
        
        print("Running post-upgrade storage rewrite (migrate phase)...")
        run_cmd(["bash", "dev/tools/migrate.sh", "--phase=migrate"])


def validate_migration():
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
    assert claim1["spec"]["warmPoolRef"]["name"] == "shadow-pool-upgrade-template", \
        f"Expected warmPoolRef name shadow-pool-upgrade-template, got {claim1['spec']['warmPoolRef']['name']}"
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
        stored_versions = json.loads(res.stdout)
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
    print("Step 3: Deleting shadow warm pools...")
    run_cmd(["kubectl", "delete", "sandboxwarmpool", "shadow-pool-upgrade-template", "-n", "default", "--ignore-not-found"])

    # Step 4: Reset storedVersions to v1alpha1 in CRD status
    print("Step 4: Resetting CRD status.storedVersions to ['v1alpha1']...")
    for crd in crds:
        patch = '{"status":{"storedVersions":["v1alpha1"]}}'
        run_cmd(["kubectl", "patch", "crd", crd, "--subresource=status", "--type=merge", "-p", patch])
        
    # Step 5: Downgrade controller and CRDs to v1alpha1
    print("Step 5: Downgrading controller and CRDs to v1alpha1...")
    if method == "kubectl":
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
    
    print("\nALL ROLLBACK VALIDATIONS PASSED SUCCESSFULLY!")

def main():
    parser = argparse.ArgumentParser(description="Run E2E migration tests for agent-sandbox")
    parser.add_argument("--image-prefix",
                        dest="image_prefix",
                        help="registry/prefix for target images. Defaults to None",
                        type=str,
                        default=None)
    parser.add_argument("--image-tag",
                        dest="image_tag",
                        help="tag for target images. Defaults to None",
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
        create_v1alpha1_objects()
        
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
        validate_migration()
        
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
