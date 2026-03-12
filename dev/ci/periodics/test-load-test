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

import os
import argparse
import shutil
import sys
import subprocess
import tempfile

repo_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
if repo_root not in sys.path:
    sys.path.insert(0, repo_root)
    
from dev.tools.shared import utils as tools_utils

def install_clusterloader2(repo_root):
    """Installs clusterloader2 if not present."""
    bin_dir = os.path.join(repo_root, "bin")
    if not os.path.exists(bin_dir):
        os.makedirs(bin_dir)
    cl2_path = os.path.join(bin_dir, "clusterloader2")
    if os.path.exists(cl2_path):
        return cl2_path
    
    print("Installing clusterloader2...")
    with tempfile.TemporaryDirectory() as tmpdirname:
        subprocess.check_call(["git", "clone", "--depth", "1", "https://github.com/kubernetes/perf-tests.git", tmpdirname])
        
        # Build from inside clusterloader2 directory as per README so go.mod is found
        build_dir = os.path.join(tmpdirname, "clusterloader2")
        cmd = ["go", "build", "-o", cl2_path, "./cmd/clusterloader.go"]
        subprocess.check_call(cmd, cwd=build_dir)
    return cl2_path

def main(args):
    """Invokes load-test in kind cluster and outputs a junit report in the ARTIFACTS dir

    The ARTIFACTS environment variable is set by prow.
    """
    image_tag = tools_utils.get_image_tag()
    result = subprocess.run([f"{repo_root}/dev/tools/create-kind-cluster", "load-test", "--recreate", "--kubeconfig", f"{repo_root}/bin/KUBECONFIG"])
    if result.returncode != 0:
        return result.returncode
    result = subprocess.run([f"{repo_root}/dev/tools/push-images", "--kind-cluster-name", "load-test", "--image-prefix", args.image_prefix, "--image-tag", image_tag, "--controller-only"])
    if result.returncode != 0:
        return result.returncode
    result = subprocess.run([f"{repo_root}/dev/tools/deploy-to-kube", "--image-prefix", args.image_prefix, "--image-tag", image_tag])
    if result.returncode != 0:
        return result.returncode
    result = subprocess.run([f"{repo_root}/dev/tools/deploy-cloud-provider"])
    if result.returncode != 0:
        return result.returncode

    cl2_path = install_clusterloader2(repo_root)
    test_config = "agent-sandbox-load-test.yaml"
    kubeconfig = os.path.join(repo_root, "bin/KUBECONFIG")

    # Create overrides file with CLI arguments
    with tempfile.NamedTemporaryFile(mode='w', delete=False) as overrides_file:
        overrides_file.write(f"CL2_REPLICAS: {args.replicas}\n")
        overrides_file.write(f"CL2_NAMESPACES: {args.namespaces}\n")
        overrides_file.write(f"CL2_QPS: {args.qps}\n")
        overrides_file.write(f"CL2_NAMESPACE_PREFIX: {args.namespace_prefix}\n")
        overrides_path = overrides_file.name

    try:
        # Run clusterloader2 from the load-test directory so relative paths in config work
        report_dir = os.path.join(repo_root, "bin")
        cmd = [cl2_path, f"--testconfig={test_config}", f"--kubeconfig={kubeconfig}", f"--testoverrides={overrides_path}", "--provider=kind", "--v=2", f"--report-dir={report_dir}"]
        print(f"Running load test: {' '.join(cmd)}")
        result = subprocess.run(cmd, cwd=os.path.join(repo_root, "dev/load-test"))
    finally:
        if os.path.exists(overrides_path):
            os.remove(overrides_path)
        # Cleanup kubeconfig and cl2 path for fresh runs
        if os.path.exists(kubeconfig):
            os.remove(kubeconfig)
        if os.path.exists(cl2_path):
            os.remove(cl2_path)

    # Always create junit file whether tests pass or fail
    artifact_dir = os.getenv("ARTIFACTS")

    if artifact_dir:
        if os.path.exists(f"{repo_root}/bin/junit.xml"):
            shutil.copy(f"{repo_root}/bin/junit.xml", f"{artifact_dir}/junit_load_test.xml")

    return result.returncode


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--image-prefix",
        dest="image_prefix",
        help="prefix for the image name. requires slash at the end if a path",
        type=str,
        default="kind.local/",
    )
    parser.add_argument(
        "--replicas",
        dest="replicas",
        help="Number of replicas per namespace",
        type=int,
        default=5,
    )
    parser.add_argument(
        "--namespaces",
        dest="namespaces",
        help="Number of namespaces",
        type=int,
        default=1,
    )
    parser.add_argument(
        "--qps",
        dest="qps",
        help="QPS for creating objects",
        type=float,
        default=10,
    )
    parser.add_argument(
        "--namespace-prefix",
        dest="namespace_prefix",
        help="Prefix for namespaces",
        type=str,
        default="agent-sandbox",
    )
    args = parser.parse_args()
    sys.exit(main(args))