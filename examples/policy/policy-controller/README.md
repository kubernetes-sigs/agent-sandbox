# User Guide: Protecting Agentic Sandboxes with Policy Controller

## 1. Overview

This guide provides step-by-step instructions for configuring a Policy Controller on a **GKE** cluster. The goal of this policy is to **prevent any user or process from granting new permissions to a ServiceAccount that is actively being used by a custom `Sandbox` resource.**

This acts as a critical security boundary, preventing accidental or malicious privilege escalation for sandboxed environments.

**How it Works:**  
The policy intercepts all `RoleBinding` and `ClusterRoleBinding` creation requests. If the request targets a `ServiceAccount` that is referenced by a running `Sandbox`, Policy Controller will **block the request and return an error**.

---

## 2. Prerequisites

Before you begin, ensure you have the following:

- `kubectl` access to a Kubernetes **GKE Standard** or **GKE Autopilot** cluster with permissions to install Helm charts.
- Helm v3+ installed on your local machine.

```bash
# Set variables to your own values and copy paste them into your active shell
CLUSTER_LOCATION=<YOUR_CLUSTER_LOCATION>
CLUSTER_NAME=<YOUR_CLUSTER_NAME>
PROJECT_ID=$(gcloud config get project)
MEMBERSHIP_NAME=agentic-sandbox-fleet
```

Make sure you have created a plain simple AP cluster beforehand or a Standard cluster. **NOTE: if you plan on using this guide with Standard GKE Cluster make sure that you enable Workload Identity during the cluster creation process. Otherwise, it will add at least 15 more minutes to deploy the Policy Controller. If you didn’t have a chance to do that and you don’t mind simply keep following the steps and they will guide you to enable the feature.**

1. Set Project Context:

```bash
gcloud config set project ${PROJECT_ID}
```

2. Enable the API:

```bash
gcloud services enable anthospolicycontroller.googleapis.com
```

3. **Register Cluster to Fleet (If your cluster is not already registered):**
The Policy Controller is designed as a fleet-level feature. Google Cloud uses the concept of a "fleet" (a logical grouping of Kubernetes clusters) to manage features that span across multiple clusters in a more centralized and consistent way. Note: this command will fail if you’re using Standard Cluster and if you didn’t enable Workload Identity. If so jump to **Step 4**.

```bash
gcloud container fleet memberships register ${MEMBERSHIP_NAME} \
  --gke-cluster=${CLUSTER_LOCATION}/${CLUSTER_NAME} \
  --enable-workload-identity \
  --project=${PROJECT_ID}
```

4. For Standard Cluster only **(Skip to Section 3 Configuration Steps if you already have enabled Workload Identity and the previous step worked)** you would need to first enable the Workload Identity feature. This operation will take 5-10 min. 

```bash
gcloud container clusters update ${CLUSTER_NAME} \
  --location=${CLUSTER_LOCATION} \
  --workload-pool=${PROJECT_ID}.svc.id.goog \
  --project=${PROJECT_ID}
```

- **Update Node Pool to use GKE Metadata Server:** Workload Identity requires node pools to use the GKE Metadata Server. You need to update each node pool in your Standard cluster. Note updating a node basically means node recreation so the time to update each node could be around 8 min per node pool (the node pool had 2 nodes).  

```bash
# List the nodes first
gcloud container node-pools list \
  --cluster=${CLUSTER_NAME} \
  --location=${CLUSTER_LOCATION} \
  --project=${PROJECT_ID}

# For each node pool name from the previous output run  
gcloud container node-pools update YOUR_NODE_POOL_NAME \
  --cluster=${CLUSTER_NAME} \
  --location=${CLUSTER_LOCATION} \
  --workload-metadata=GKE_METADATA \
  --project=${PROJECT_ID}
```

After this is done you can go back to **Step 3** and create the fleet. 

---

## 3. Configuration Steps

### Step 1: Install [Policy Controller](https://cloud.google.com/kubernetes-engine/enterprise/policy-controller/docs/how-to/installing-policy-controller)


```bash
gcloud container fleet policycontroller enable \
  --memberships=${MEMBERSHIP_NAME} \
  --project=${PROJECT_ID}
```

Wait for the installation to be complete and you can check the status with. Look for the state: `ACTIVE` for the admission and audit components. It takes around 30 seconds to 1 min to appear `ACTIVE`: 

```bash
gcloud container fleet policycontroller describe \
  --memberships=${MEMBERSHIP_NAME} \
  --project=${PROJECT_ID}
```

### Step 2: Verify Custom Resource Definitions (CRDs)

Ensure that the CRDs are present in your cluster. Should be >30 CRDs installed, it takes a couple of seconds for all CRDs to be installed. 

```bash
# This command should return the names of the CRDs installed by the Policy controller	
kubectl get crds | grep gatekeeper
```

If either CRD is not found, stop and resolve that issue before proceeding.

### Step 3: Define the Config and understanding Rego logic

- The `ConstraintTemplate` uses Rego code that tries to look up other resources in the cluster. The custom template that we are going to use, uses `data.inventory`, which requires referential constraints to be enabled.

```bash
gcloud container fleet policycontroller update \
  --memberships=${MEMBERSHIP_NAME} \
  --referential-rules \
  --project=${PROJECT_ID}
```

Specifically, it uses `data.inventory` to find `Pod` objects, this is from the Rego logic:

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

### Step 4: Define the ConstraintTemplate

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

### Step 5: Define the Constraint

The `Constraint` instantiates the template and specifies which resources it should apply to.

```bash
kubectl apply -f constraint-sandbox-binding.yaml
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
apiVersion: agents.x-k8s.io/v1alpha1
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

## 5. Troubleshooting

The order in which you apply the gatekeeper yaml files (`Config` -> `ConstraintTemplate` -> `Constraint`) is important. If for some reason you see an error when you describe the `ConstrainTemplate` and/or you aren’t able to apply the `Constraint`. Do the following: 

```bash
# delete config
kubectl delete config config -n gatekeeper-system

# delete constrainttemplate
kubectl delete constrainttemplate k8spreventsandboxserviceaccountbinding

# delete the gatekeeper-controller-manager pod so that it restarts
kubectl delete pod gatekeeper-controller-manager-xxxx-xxxx -n gatekeeper-system	
```

Wait for the gatekeeper-controller-manager pod to be up and running and then apply the `Config`, `ConstraintTemplate` (verify again that there’s no errors in the status when you describe it) and `Constraint`. 


## 6. Cleanup

```bash
# delete the constraint
kubectl delete K8sPreventSandboxServiceAccountBinding prevent-sandbox-sa-binding

# delete constrainttemplate
kubectl delete constrainttemplate k8spreventsandboxserviceaccountbinding

# delete config
kubectl delete config config -n gatekeeper-system

# Disable Policy Controller Feature
gcloud container fleet policycontroller disable \
  --memberships=${MEMBERSHIP_NAME} \
  --project=${PROJECT_ID}
```

Verify uninstallation from Cluster, it might take a couple of minutes to actually start seeing the “Active” Status becoming “Terminating”

```bash
# you should see the status changing from Active to Terminating and then you should no longer see the namespace i.e. you should see namespace not found error
kubectl get namespace gatekeeper-system


# Check CRDs
kubectl get crds | grep gatekeeper.sh

# If any CRDs are remaining you can delete them manually, for example:
kubectl delete crd configs.config.gatekeeper.sh
```
Unregister Cluster from Fleet by removing the cluster’s membership from the fleet. 

```bash
gcloud container fleet memberships unregister ${MEMBERSHIP_NAME} \
  --gke-cluster=${CLUSTER_LOCATION}/${CLUSTER_NAME} \
  --project=${PROJECT_ID}
```