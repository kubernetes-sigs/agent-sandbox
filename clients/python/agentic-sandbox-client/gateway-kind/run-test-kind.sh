#!/bin/bash
# Copyright 2025 The Kubernetes Authors.
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


set -e

export KIND_CLUSTER_NAME="agent-sandbox"

# Get the image tag using a temporary Python script
REPO_ROOT=$(git rev-parse --show-toplevel)

TEMP_PY_SCRIPT=$(mktemp /tmp/get_tag_XXXXXX.py)
cat <<EOF > "$TEMP_PY_SCRIPT"
import sys
sys.path.append('$REPO_ROOT/dev/tools/shared')
from utils import get_image_tag
print(get_image_tag())
EOF

IMAGE_TAG=$(python3 "$TEMP_PY_SCRIPT")

rm "$TEMP_PY_SCRIPT"
echo "Using image tag: $IMAGE_TAG"


export SANDBOX_ROUTER_IMG="kind.local/sandbox-router:${IMAGE_TAG}"
export SANDBOX_PYTHON_RUNTIME_IMG="kind.local/python-runtime-sandbox:${IMAGE_TAG}"

# Start virtual environment and install dependencies first so we have yaml library available
echo "Starting virtual environment for Python client and install agentic sandbox client..."
cd "$REPO_ROOT/clients/python/agentic-sandbox-client"
python3 -m venv .venv
source .venv/bin/activate
pip install -e .

# following develop guide to make and deploy agent-sandbox to kind cluster
cd "$REPO_ROOT"
make build
make deploy-kind EXTENSIONS=true
make deploy-cloud-provider-kind

cd "$REPO_ROOT/clients/python/agentic-sandbox-client/gateway-kind"
echo "Applying CRD for template - Sandbox claim will be applied by the sandbox client in python code"
python3 "$REPO_ROOT/dev/tools/mutate_yaml.py" template python-sandbox-template.yaml --image "${SANDBOX_PYTHON_RUNTIME_IMG}" | kubectl apply -f -

cd "$REPO_ROOT/clients/python/agentic-sandbox-client/sandbox-router"
echo "Applying CRD for router template"
python3 "$REPO_ROOT/dev/tools/mutate_yaml.py" router sandbox_router.yaml --image "${SANDBOX_ROUTER_IMG}" --allow-unauthenticated | kubectl apply -f -
kubectl rollout status deployment/sandbox-router-deployment --timeout=60s

cd "$REPO_ROOT/clients/python/agentic-sandbox-client/gateway-kind"
echo "Setting up Cloud Provider Kind Gateway in the kind cluster..."
echo "Applying Gateway configuration..."
kubectl apply -f gateway-kind.yaml
kubectl wait --for=condition=Programmed=True gateway/kind-gateway --timeout=60s

# Cleanup function
cleanup() {
    echo "Cleaning up virtual environment..."
    deactivate || true
    rm -rf "$REPO_ROOT/clients/python/agentic-sandbox-client/.venv"

    cd "$REPO_ROOT"
    echo "Cleaning up cloud provider kind..."
    make kill-cloud-provider-kind
    
    echo "Deleting kind cluster..."
    make delete-kind || true

    echo "Cleanup completed."
}

trap cleanup EXIT

cd "$REPO_ROOT/clients/python/agentic-sandbox-client"
echo "========= $0 - Running the Python client tester... ========="
python3 ./test_client.py --gateway-name kind-gateway
echo "========= $0 - Finished running the Python client with gateway and router tester. ========="

echo "Test finished."

