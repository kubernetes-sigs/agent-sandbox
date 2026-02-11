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

# End-to-end test for LangChain DeepAgents with agent-sandbox on Kind
#
# Prerequisites:
#   - Docker installed and running
#   - kind installed
#   - kubectl installed
#   - ANTHROPIC_API_KEY (or OPENAI_API_KEY or GOOGLE_API_KEY) set
#
# Usage:
#   ./run-test-kind.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CLUSTER_NAME="agent-sandbox"
NAMESPACE="default"

echo "=== LangChain DeepAgents E2E Test ==="
echo ""

# Check for API key
if [[ -z "${ANTHROPIC_API_KEY:-}" ]] && [[ -z "${OPENAI_API_KEY:-}" ]] && [[ -z "${GOOGLE_API_KEY:-}" ]]; then
    echo "Error: No LLM API key found. Set one of:"
    echo "  - ANTHROPIC_API_KEY"
    echo "  - OPENAI_API_KEY"
    echo "  - GOOGLE_API_KEY"
    exit 1
fi

# Function to cleanup on exit
cleanup() {
    echo ""
    echo "=== Cleanup ==="

    # Delete sandbox claim if exists
    kubectl delete sandboxclaim --all -n "${NAMESPACE}" --ignore-not-found 2>/dev/null || true

    # Delete sandbox template
    kubectl delete -f "${SCRIPT_DIR}/sandbox-template.yaml" --ignore-not-found 2>/dev/null || true

    echo "Cleanup complete"
}

# Set trap for cleanup
trap cleanup EXIT

# Step 1: Deploy Kind cluster and controller (using project Makefile)
echo "=== Step 1: Deploy Kind Cluster ==="
cd "${PROJECT_ROOT}"

if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Kind cluster '${CLUSTER_NAME}' already exists"
    export KUBECONFIG="$(kind get kubeconfig-path --name=${CLUSTER_NAME} 2>/dev/null || echo "${HOME}/.kube/config")"
else
    echo "Creating Kind cluster and deploying controller..."
    make deploy-kind EXTENSIONS=true
fi

# Get kubeconfig
export KUBECONFIG="${PROJECT_ROOT}/bin/KUBECONFIG"
if [[ ! -f "${KUBECONFIG}" ]]; then
    KUBECONFIG="$(kind get kubeconfig-path --name=${CLUSTER_NAME} 2>/dev/null || echo "${HOME}/.kube/config")"
fi
echo "Using KUBECONFIG: ${KUBECONFIG}"

# Step 2: Build and load sandbox runtime image
echo ""
echo "=== Step 2: Build Sandbox Runtime Image ==="
cd "${PROJECT_ROOT}/examples/python-runtime-sandbox"
docker build -t sandbox-runtime:latest .
kind load docker-image sandbox-runtime:latest --name "${CLUSTER_NAME}"

# Step 3: Deploy SandboxTemplate
echo ""
echo "=== Step 3: Deploy SandboxTemplate ==="
cd "${SCRIPT_DIR}"
kubectl apply -f sandbox-template.yaml
echo "Waiting for SandboxTemplate to be ready..."
sleep 2

# Step 4: Install Python dependencies
echo ""
echo "=== Step 4: Install Python Dependencies ==="
cd "${SCRIPT_DIR}"

# Install dependencies using uv
if command -v uv &> /dev/null; then
    uv sync
else
    echo "Warning: uv not found, using pip"
    pip install -e "${PROJECT_ROOT}/clients/python/agentic-sandbox-client"
    pip install -e "${PROJECT_ROOT}/clients/python/langchain-agent-sandbox"
    pip install langchain-anthropic langchain-openai langchain-google-genai
fi

# Step 5: Run the agent with a test query
echo ""
echo "=== Step 5: Run DeepAgent Test ==="

# Set environment variables for tunnel mode
export LANGCHAIN_SANDBOX_TEMPLATE="python-deepagent"
export LANGCHAIN_NAMESPACE="${NAMESPACE}"
export LANGCHAIN_USE_TUNNEL=1

# Run a simple test query
TEST_QUERY="Create a file called hello.txt with the content 'Hello from DeepAgent!' and then read it back"

echo "Test query: ${TEST_QUERY}"
echo ""

if command -v uv &> /dev/null; then
    uv run python main.py --query "${TEST_QUERY}"
else
    python main.py --query "${TEST_QUERY}"
fi

echo ""
echo "=== Test Complete ==="
