# User Guide: Protecting Agentic Sandboxes with Kyverno ValidatingPolicy

## 1. Overview

This guide provides step-by-step instructions for configuring a Kyverno
ValidatingPolicy on a Kubernetes cluster. The goal of this policy is to prevent
any user or process from granting new permissions to a ServiceAccount that is
actively being used by a custom Sandbox resource, whether permissions are
granted directly to that ServiceAccount or indirectly through the built-in
ServiceAccount groups.

This acts as a critical security boundary, preventing accidental or malicious
privilege escalation for sandboxed environments.

How it works:
The policy intercepts RoleBinding and ClusterRoleBinding create and update
requests. If the request targets a ServiceAccount referenced by a
Sandbox-owned Pod, or a Group subject that would include that ServiceAccount
(`system:serviceaccounts` or `system:serviceaccounts:<namespace>`), the request
is denied.

Performance note:
For `system:serviceaccounts` (cluster-wide) Group subjects, the policy may need
to evaluate Pods across namespaces. In very large clusters, this can increase
admission latency and may contribute to webhook timeouts under heavy load.
If this becomes an issue, consider reducing use of cluster-wide
`system:serviceaccounts` bindings, or adapting this sample with a caching
strategy (for example, GlobalContextEntry) to reduce per-request lookup cost.

---

## 2. Prerequisites

Before you begin, ensure you have the following:

- kubectl access to a Kubernetes cluster.
- Helm v3+ installed.
- Kyverno installed in the cluster.
- Agent Sandbox CRDs/controller installed if you want to create real Sandbox
  resources for manual verification.

Install Kyverno:

```bash
# 1. Add the Kyverno Helm repository
helm repo add kyverno https://kyverno.github.io/kyverno/

# 2. Update local Helm repositories
helm repo update

# 3. Install Kyverno
helm install kyverno kyverno/kyverno --namespace kyverno --create-namespace
```

Verify Kyverno is running:

```bash
kubectl get pods -n kyverno
```

---

## 3. Configuration Steps

### Step 1: Grant RBAC required by policy reporting checks

This example includes an RBAC grant used by the test flow so the policy can
report ready conditions.

```bash
kubectl apply -f .chainsaw-tests/setup-rbac.yaml
```

### Step 2: Apply the ValidatingPolicy

```bash
kubectl apply -f prevent-sandbox-sa-binding.yaml
```

### Step 3: Verify policy readiness

```bash
kubectl get validatingpolicy prevent-sandbox-sa-binding -o yaml
```

Look for ready status and successful conditions such as WebhookConfigured and
RBACPermissionsGranted.

---

## 4. Testing and Verification

### A. Set up the test scenario

```bash
kubectl apply -f .chainsaw-tests/setup-sa.yaml
kubectl apply -f .chainsaw-tests/setup-sandbox.yaml
```

### B. Trigger the policy

```bash
kubectl apply -f .chainsaw-tests/bad-rolebinding.yaml
```

The same deny behavior also applies if you bind one of the built-in
ServiceAccount groups that would include the active Sandbox ServiceAccount, such
as `system:serviceaccounts:sandbox-ns` or cluster-wide `system:serviceaccounts`.

### C. Check expected outcome 

The request should be denied by admission.
```
Error from server: error when creating "examples/policy/kyverno/.chainsaw-tests/bad-rolebinding.yaml": admission webhook "vpol.validate.kyverno.svc-fail" denied the request: Policy prevent-sandbox-sa-binding failed: Binding denied: one or more subjects reference a ServiceAccount or ServiceAccount group that is actively used by a Sandbox-owned Pod. ServiceAccounts in use by Pods controlled by a Sandbox CR (agents.x-k8s.io) must not be granted additional RBAC bindings to prevent privilege escalation in sandboxed environments.
```

### Scenario 2: Binding to an unused ServiceAccount (should be allowed)

```bash
kubectl apply -f .chainsaw-tests/setup-other-sa.yaml
kubectl apply -f .chainsaw-tests/good-rolebinding.yaml
```

Expected: RoleBinding is created.

### Scenario 3: ServiceAccount used by a non-Sandbox Pod (should be allowed)

```bash
kubectl delete -f .chainsaw-tests/setup-sandbox.yaml
kubectl apply -f .chainsaw-tests/barepod.yaml
kubectl apply -f .chainsaw-tests/bad-rolebinding.yaml
```

Expected: RoleBinding is created after Sandbox ownership is removed.

### Scenario 4: ServiceAccount Group subject covering an active Sandbox ServiceAccount (should be denied)

If a RoleBinding or ClusterRoleBinding uses a Group subject like
`system:serviceaccounts:sandbox-ns` or `system:serviceaccounts`, the request is
also denied whenever that group would include a ServiceAccount currently used by
a Sandbox-owned Pod.

---

## 5. Run Automated Chainsaw Tests

Run the full test suite:

```bash
chainsaw test --test-dir .chainsaw-tests
```

The test file is:

- .chainsaw-tests/chainsaw-test.yaml

Step mapping in the test:

- step-01: apply RBAC + policy and assert readiness
- step-02: assert deny for active Sandbox ServiceAccount and matching ServiceAccount groups
- step-03: assert allow for unused ServiceAccount and a binding with no subjects
- step-04: remove Sandbox owner, create bare Pod, then allow binding

---

## 6. Cleanup

```bash
kubectl delete -f .chainsaw-tests/good-rolebinding.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/bad-rolebinding.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/barepod.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/setup-other-sa.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/setup-sandbox.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/setup-sa.yaml --ignore-not-found
kubectl delete -f prevent-sandbox-sa-binding.yaml --ignore-not-found
kubectl delete -f .chainsaw-tests/setup-rbac.yaml --ignore-not-found
```

Optional: uninstall Kyverno

```bash
helm uninstall kyverno -n kyverno
```
