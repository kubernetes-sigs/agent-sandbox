---
title: "gVisor"
linkTitle: "gVisor"
weight: 2
description: >
  This guide shows how to run [Agent Sangbox](https://github.com/kubernetes-sigs/agent-sandbox) with the [gVisor](https://gvisor.dev) runtime using kind as a cluster.
---

## Prerequisites

* [kind](https://kind.sigs.k8s.io/docs/user/quick-start/)
* [kubectl](https://kubernetes.io/docs/tasks/tools/)

## Create a kind cluster

Create a Kind cluster and deploy the agent controller by following this [installation tutorial](../../installation/_index.md).

## Install gVisor

Execute these commands to access the cluster's node. We will install gVisor there.
```sh
CLUSTER_NAME="agent-sandbox"
NODE_CONTAINER=$(docker ps --filter "name=${CLUSTER_NAME}-control-plane" --format "{{.Names}}")
echo $NODE_CONTAINER
```

Update and install crucial binaries inside your node by running:
```sh
docker exec -i "${NODE_CONTAINER}" bash -c "apt-get update -qq && apt-get install -y -qq sudo curl wget gnupg2 ca-certificates"
```

These commands install `runsc` and `shim` inside the node container:
```sh
docker exec -i "${NODE_CONTAINER}" bash -c '
set -e
ARCH=$(uname -m)
URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
cd /tmp
wget -q ${URL}/runsc ${URL}/runsc.sha512 ${URL}/containerd-shim-runsc-v1 ${URL}/containerd-shim-runsc-v1.sha512
sha512sum -c runsc.sha512 -c containerd-shim-runsc-v1.sha512
chmod a+rx runsc containerd-shim-runsc-v1
mv runsc containerd-shim-runsc-v1 /usr/local/bin
rm -f *.sha512
/usr/local/bin/runsc install || true
'
```

Configuring containerd to use gVisor runtime:
```sh
docker exec -i "${NODE_CONTAINER}" bash -c '
set -e
# Back up the old config
cp /etc/containerd/config.toml /etc/containerd/config.toml.bak.$(date +%s) || true

# Detect containerd config version
VERSION_LINE=$(grep "^version" /etc/containerd/config.toml || echo "")
if echo "$VERSION_LINE" | grep -q "3"; then
  echo "Detected containerd config version 3, writing compatible config..."
  cat <<EOF >/etc/containerd/config.toml
version = 3

[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "registry.k8s.io/pause:3.9"
    [plugins."io.containerd.grpc.v1.cri".containerd]
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
          runtime_type = "io.containerd.runsc.v1"
EOF
else
  echo "Detected containerd config version 2 or missing version, writing legacy config..."
  cat <<EOF >/etc/containerd/config.toml
version = 2
[plugins."io.containerd.runtime.v1.linux"]
  shim_debug = true
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF
fi

# Restart containerd to apply changes
(systemctl restart containerd 2>/dev/null || service containerd restart 2>/dev/null || pkill -HUP containerd)
sleep 2
'
```

Creating RuntimeClass to be able to use `gVsior` as a `RuntimeClass`:
```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
```

## Testing

Run this command to create an ampty Sandbox that uses `gVisor` as a RuntimeClass:
```sh
kubectl apply -f gvisor-empty-sandbox.yaml
```

## Cleanup

```sh
kind delete cluster --name agent-sandbox
```
