# Managed Sandbox Example

This example is a prototype for hosting multiple bubblewrap-isolated sandbox tenants inside shared pool pods. It is intentionally kept outside the core controller and outside the supported extension APIs while the design is discussed.

The example includes:

- `api/v1alpha1`: an example-only `ManagedSandbox` CRD under `examples.agents.x-k8s.io`.
- `controllers/managedsandbox`: a controller that provisions pool pods, binds sandboxes to pool pods, and asks the pod-side agent to create tenants.
- `internal/pool`: controller-side pool pod selection, provisioning, and orphan cleanup helpers.
- `pod-agent`: the Rust pod-side agent and worker used to create bubblewrap tenants and expose SSH.
- `proto` and `clients/agentsandbox`: pod-agent and worker gRPC APIs plus generated Go bindings.

## Build

Build and load the pod-agent image from this directory:

```sh
docker build -t kind.local/pod-agent:dev -f pod-agent/Dockerfile .
kind load docker-image kind.local/pod-agent:dev
```

Build and load the example controller image:

```sh
docker build -t kind.local/managed-sandbox-controller:dev -f controller.Dockerfile .
kind load docker-image kind.local/managed-sandbox-controller:dev
```

## Install

Apply the example CRD, RBAC, and in-cluster controller:

```sh
kubectl apply -k config
kubectl rollout status deployment/managed-sandbox-example-controller
```

Create a sample managed sandbox:

```sh
kubectl apply -f config/sample-managedsandbox.yaml
kubectl wait managedsandbox/smoke --for=condition=Ready=True --timeout=60s
```

The controller must run in the cluster for this prototype: it dials the
pod-agent on the pool pod's Pod IP, which is usually not reachable from the
host when using kind.

The controller writes `status.sshHost`, `status.sshPort`, and `status.sshSecretName` when the pod-agent creates the tenant. See `design-notes.md` for the current prototype state and known gaps.

## SSH

In kind, connect from a temporary pod because `status.sshHost` is a Pod IP:

```sh
SUID=$(kubectl get managedsandbox smoke -o jsonpath='{.metadata.uid}')
HOST=$(kubectl get managedsandbox smoke -o jsonpath='{.status.sshHost}')
PORT=$(kubectl get managedsandbox smoke -o jsonpath='{.status.sshPort}')
SECRET=$(kubectl get managedsandbox smoke -o jsonpath='{.status.sshSecretName}')
TOKEN=$(kubectl get secret "$SECRET" -o jsonpath='{.data.token}' | base64 -d)

kubectl run -it --rm sshtest \
  --image=alpine \
  --restart=Never \
  --env=SSHPASS="$TOKEN" \
  -- sh -lc "apk add --no-cache openssh-client sshpass >/dev/null && exec sshpass -e ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p $PORT $SUID@$HOST"
```

Or run one command inside the sandbox:

```sh
kubectl run -it --rm sshtest \
  --image=alpine \
  --restart=Never \
  --env=SSHPASS="$TOKEN" \
  -- sh -lc "apk add --no-cache openssh-client sshpass >/dev/null && sshpass -e ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p $PORT $SUID@$HOST 'pwd && id && ls -la'"
```
