# User Guide: Protecting Agentic Sandboxes with OPA Gatekeeper

## 1. Overview

This guide provides step-by-step instructions for configuring a OPA Gatekeeper security policy on a Kubernetes cluster. The goal of this policy is to **prevent any user or process from granting new permissions to a ServiceAccount that is actively being used by a custom `Sandbox` resource.**

This acts as a critical security boundary, preventing accidental or malicious privilege escalation for sandboxed environments.

**How it Works:**  
The policy intercepts all `RoleBinding` and `ClusterRoleBinding` creation requests. If the request targets a `ServiceAccount` that is referenced by a running `Sandbox`, OPA Gatekeeper will **block the request and return an error**.

---

## 2. Prerequisites

Before you begin, ensure you have the following:

- `kubectl` access to a Kubernetes **Standard** cluster with permissions to install Helm charts.
- Helm v3+ installed on your local machine.

---

## 3. Configuration Steps

### Step 1: Install [OPA Gatekeeper](https://open-policy-agent.github.io/gatekeeper/website/docs/install/)

Use the following Helm commands to deploy it to your cluster.

```bash
# 1. Add the official OPA Gatekeeper Helm repository
helm repo add gatekeeper https://open-policy-agent.github.io/gatekeeper/charts

# 2. Update your local Helm repositories
helm repo update

# 3. Install OPA into its own namespace
helm install gatekeeper/gatekeeper --name-template=gatekeeper --namespace gatekeeper-system --create-namespace
```

You can verify the installation by checking the pods and CRDs in the gatekeeper-system namespace:

```bash
kubectl get pods -n gatekeeper-system
```

All pods should be in the `Running` state.

### Step 2: Verify Custom Resource Definitions (CRDs)

Ensure that the Gatekeeper CRDs are present in your cluster:

```bash
kubectl get crds | grep gatekeeper
```

If either CRD is not found, stop and resolve that issue before proceeding.

### Step 3: Define the ConstraintTemplate

The ConstraintTemplate provides the Rego logic that Gatekeeper will use to enforce the policy. This template looks for `Pods` using the targeted `ServiceAccount` and checks if they are owned by a `Sandbox` CR.

Apply the template:

```bash
kubectl apply -f template-sandbox-binding.yaml
```

This creates a new CRD called `K8sPreventSandboxServiceAccountBinding` that Gatekeeper will recognize. You can verify it by running the following:

```bash
kubectl get crd k8spreventactiveserviceaccountbinding

# You should see a crd like the following:
k8spreventactiveserviceaccountbinding.constraints.gatekeeper.sh   
```

### Step 4: Define the Constraint

The `Constraint` instantiates the template and specifies which resources it should apply to.

```bash
kubectl apply -f constraint-sandbox-binding.yaml
```

### Step 5: Define the Config and understanding Rego logic

- The `ConstraintTemplate` uses Rego code that tries to look up other resources in the cluster. Specifically, it uses `data.inventory` to find `Pod` objects, this is from the Rego logic:

```bash
# Find a pod using this ServiceAccount in the same namespace
pod := data.inventory.namespace[sa_namespace]["v1"]["Pod"][_]
```

- This line of code means: "Iterate through all Pods in the same namespace as the ServiceAccount in the RoleBinding/ClusterRoleBinding."
- `data.inventory` Comes from a Cache: Gatekeeper doesn't query the Kubernetes API server live for every admission request when data.inventory is used. This would be too slow and overload the API server. Instead, it maintains an in-memory cache of certain Kubernetes resources.
- The `Config` Resource Populates the Cache: The `Config` object in the `gatekeeper-system` namespace, specifically the `spec.sync.syncOnly` section, tells Gatekeeper which resources to watch and put into its cache.

Apply the config:
```bash
kubectl apply -f config-sandbox-binding.yaml
```

Gatekeeper will now start enforcing this policy on any new or updated `RoleBinding` or `ClusterRoleBinding` resources.

## 4. Testing and Verification

To confirm the policy is working, simulate an attempt to grant new permissions to a protected `ServiceAccount`.

### A. Set Up the Test Scenario

Apply the `Namespace`, `ServiceAccount` and `Sandbox` resources to the cluster.

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: sandbox-ns
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sandbox-sa
  namespace: sandbox-ns
---
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: sandbox-example
  namespace: sandbox-ns
spec:
  podTemplate:
    metadata:
      labels:
        sandbox: my-sandbox
      annotations:
        test: "yes"
    spec:
      serviceAccountName: sandbox-sa
      containers:
      - name: my-container
        image: busybox
        command: ["/bin/sh", "-c", "sleep 3600"]
EOF
```


### B. Trigger the Policy (Expected Failure)

Apply a `RoleBinding` that should fail

```bash
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: test-forbidden-binding
  namespace: sandbox-ns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: view
subjects:
- kind: ServiceAccount
  name: sandbox-sa
  namespace: sandbox-ns
EOF
```

### C. Check the Expected Outcome
You should see an error like:
```bash
Error from server (Forbidden): error when creating "STDIN": admission webhook "validation.gatekeeper.sh" denied the request: [prevent-active-sa-binding] RoleBinding cannot be created/updated: ServiceAccount 'sandbox-ns/sandbox-sa' is in use by Pod 'sandbox-ns/sandbox-example', which is controlled by Sandbox CR 'sandbox-example'
```

If you see this error, your security policy is successfully configured and enforced. ✅


#### Scenario 2: Binding to an unused ServiceAccount (Should be ALLOWED)
Try to bind to `another-sa` in `test-ns`, assuming no `Sandbox` Pod uses it.

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: test-ns
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: another-sa
  namespace: test-ns
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: test-allow
  namespace: test-ns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: view
subjects:
- kind: ServiceAccount
  name: another-sa
  namespace: test-ns
EOF
```

You should see:

```bash
rolebinding.rbac.authorization.k8s.io/test-allow created
```

#### Scenario 3: Binding to a ServiceAccount used by a NON-Sandbox Pod (Should be ALLOWED)
If `sandbox` in `sandbox-ns` is used by a `Pod` not managed by an `Sandbox` CR (e.g., a standalone `Pod` or one managed by a `Deployment`), the binding should also be allowed, because the policy specifically checks for `ownerReferences` of kind `Sandbox`.


#### Summary of How it Works
The Rego code in the `ConstraintTemplate` intercepts the creation/update of `RoleBindings` and `ClusterRoleBindings`. For each `ServiceAccount` subject, it queries Gatekeeper's in-memory cache (data.inventory) for any Pods in the specified namespace that use this `ServiceAccount`. It then filters this list to include only `Pods` whose `metadata.ownerReferences` indicate they are owned by an `Sandbox` resource from the `agents.x-k8s.io` API group. If any such active `Pods` are found, the binding is rejected.
This setup effectively prevents assigning potentially elevated permissions to `ServiceAccounts` that are actively in use by your sandboxed workloads.


## 5. Uninstall

```bash
helm delete gatekeeper --namespace gatekeeper-system

# helm won't remove the crds so you have to run the following 
kubectl delete crd -l gatekeeper.sh/system=yes
```