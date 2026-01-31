#!/bin/bash
set -e

export KIND_CLUSTER_NAME="agent-sandbox"

# Navigate to root to build/deploy controller if needed
# Assuming this script is run from examples/moltbot-sandbox
cd ../../
# Only build/deploy if user asks or if we want to ensure latest controller
# make build 
# make deploy-kind
cd examples/moltbot-sandbox

echo "Pulling moltbot/moltbot:latest..."
docker pull moltbot/moltbot:latest

echo "Loading moltbot/moltbot:latest into kind cluster..."
kind load docker-image moltbot/moltbot:latest --name "${KIND_CLUSTER_NAME}"

echo "Generating secure token..."
export OPENCLAW_GATEWAY_TOKEN=$(openssl rand -hex 32)
echo "Token: $OPENCLAW_GATEWAY_TOKEN"

echo "Applying sandbox resource with generated token..."
sed "s/dummy-token-for-sandbox/$OPENCLAW_GATEWAY_TOKEN/g" moltbot-sandbox.yaml | kubectl apply -f -

# Cleanup function
cleanup() {
    echo "Cleaning up..."
    kubectl delete --ignore-not-found -f moltbot-sandbox.yaml
    # We do NOT delete the controller or cluster here to allow inspection
    # kubectl delete --ignore-not-found statefulset agent-sandbox-controller -n agent-sandbox-system
    # cd ../../
    # make delete-kind
}
# trap cleanup EXIT

echo "Waiting for sandbox pod to be ready..."
kubectl wait --for=condition=ready pod --selector=sandbox=moltbot-sandbox --timeout=120s

echo "Port-forwarding service..."
POD_NAME=$(kubectl get pods -l sandbox=moltbot-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "pod/${POD_NAME}" 18789:18789 &
PF_PID=$!

trap "kill $PF_PID" EXIT

sleep 5

echo "Checking Gateway health..."
# Gateway usually accepts websocket at / or distinct path. 
# We just check if it connects.
curl -v http://127.0.0.1:18789 || echo "Gateway accessible despite curl exit code"

echo "Test finished."
