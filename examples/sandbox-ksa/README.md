# Sandbox with Kubernetes Service Account (KSA)

This example demonstrates how to configure a Sandbox with a dedicated Kubernetes Service Account (KSA) for identity and access control.

## Overview

Each sandboxed pod can use a distinct KSA, allowing them to have distinct identities and permissions within the Kubernetes cluster. This is useful for:

- **Multi-tenant scenarios**: Different sandboxes can have different levels of access to cluster resources
- **Security isolation**: Limiting what each sandbox can do via RBAC
- **Identity-based access**: Allowing sandboxes to authenticate to the Kubernetes API with specific identities

## Files

- `sandbox-ns.yaml` - Creates the namespace for the sandbox
- `sandbox-sa.yaml` - Creates the ServiceAccount that the sandbox pod will use
- `sandbox.yaml` - Creates the Sandbox with the KSA configuration

## Usage

### 1. Apply the resources

```sh
kubectl apply -f sandbox-ns.yaml
kubectl apply -f sandbox-sa.yaml
kubectl apply -f sandbox.yaml
```

### 2. Verify the sandbox is running

```sh
kubectl get sandbox -n sandbox-ns
kubectl get pods -n sandbox-ns
```

### 3. Access the sandbox pod

```sh
kubectl exec -it sandbox-example -n sandbox-ns -- /bin/sh
```

### 4. Verify the service account identity

Inside the sandbox pod, you can verify the service account:

```sh
cat /var/run/secrets/kubernetes.io/serviceaccount/token
cat /var/run/secrets/kubernetes.io/serviceaccount/namespace
```

## Cleanup

```sh
kubectl delete -f sandbox.yaml
kubectl delete -f sandbox-sa.yaml
kubectl delete -f sandbox-ns.yaml
```

## Customization

To use a different service account, update the `serviceAccountName` field in `sandbox.yaml`:

```yaml
spec:
  podTemplate:
    spec:
      serviceAccountName: your-custom-sa
```

You can also bind RBAC rules to the service account to grant specific permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sandbox-role
  namespace: sandbox-ns
rules:
- apiGroups: [""]
  resources: ["pods", "services"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: sandbox-rolebinding
  namespace: sandbox-ns
subjects:
- kind: ServiceAccount
  name: your-sandbox-sa
  namespace: sandbox-ns
roleRef:
  kind: Role
  name: sandbox-role
  apiGroup: rbac.authorization.k8s.io
```
