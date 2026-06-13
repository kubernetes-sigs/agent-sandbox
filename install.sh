set -e

echo "Installing Agent Sandbox..."

# Check dependencies
command -v kubectl >/dev/null 2>&1 || {
  echo "kubectl is required but not installed."
  exit 1
}

command -v kind >/dev/null 2>&1 || {
  echo "kind is required but not installed."
  exit 1
}

CLUSTER_NAME="agent-sandbox"

echo "Creating kind cluster: $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" || true

echo "Building project..."
make build

echo "Loading images into kind..."
./dev/tools/push-images --image-prefix=kind.local/ --kind-cluster-name=$CLUSTER_NAME

echo "Deploying to cluster..."
./dev/tools/deploy-to-kube --image-prefix=kind.local/

echo ""
echo "Agent Sandbox installed successfully."
echo ""
echo "To verify:"
echo "kubectl get pods -A"