# Dynamic Storage Late-Binding for GKE Sandbox Warm Pools

Agents generally use one of three file storage access modes:

- **Private Isolated Workspaces:** An agent starts up with a private isolated storage directory it has sole read/write access to.
- **Collaborative Workspaces:** Multiple coordinating agent pods mount the exact same shared subdirectory in Read-Write (RW) mode to collaboratively update files in real-time.
- **Exploration Branching Workspaces**: Agent pods access a base template in Read-Only (RO) mode to avoid altering the master copy, either by routing new writes to a separate local emptyDir scratchpad / private persistent path or by copying the template files directly into a private writable workspace on startup. 

This guide explains how to dynamically bind pre-allocated tenant storage (from a shared RWX Filestore instance) into pre-warmed GKE Sandbox (gVisor) pods upon claim, enabling subsecond warm sandbox claim latency. We use Filestore Regional for cost efficiency and demonstrate the Private Isolated Workspace agent data access mode, but other RWX storage like [Managed Lustre](https://docs.cloud.google.com/managed-lustre/docs/lustre-csi-driver-new-volume) can be used here as well if your agent workloads need extremely high aggregate throughput. Instructions on how to adapt this for Collaborative or Exploration Branching workspaces data access modes are included in [Trigger Dynamic Storage Binding (HTTP Request)](#trigger-dynamic-storage-binding-http-request) or [Restore from Snapshot (Claiming and Binding)](#restore-from-snapshot-claiming-and-binding) below. 

**Note on Storage Limits:** Because this architecture dynamically bind-mounts a shared master PVC after the pod has started, standard Kubernetes resource.limits cannot be used to enforce per-agent storage quotas. If an orchestrator does not implement custom directory monitoring, a single agent could potentially fill the entire shared volume and cause a Denial of Service.

Kubernetes does not allow attaching persistent volumes dynamically to pre-created warm pool pods without restarting them. To bypass this constraint and avoid pod restarts to reduce sandbox claim latency, we utilize a custom privileged **Storage Node Daemon** running as a `DaemonSet`. When a sandbox is claimed, a late-bind HTTP request is sent to the local node daemon, which bind-mounts the tenant's Filestore subdirectory directly into the pod's `emptyDir` mount path on the host. Since the container volume mount uses `HostToContainer` propagation, the dynamically bound storage instantly becomes accessible inside the running sandbox container.

**About this Guide: Manual Commands vs Production Architecture**

This guide is designed to walk you through the core mechanics of late-binding storage in GKE Agent Sandbox environments.

- **This Guide:** To make the underlying mechanics transparent, the steps in this guide use manual terminal commands (kubectl exec, curl, and kubectl patch) to simulate orchestrator actions.
- **Production Implementation:** In a production system, these manual steps must be fully automated by a dedicated controller or platform orchestrator. To help you transition from this POC to production, look for the highlighted **Production Automation Note** sections throughout the guide. 
# Prerequisites and Cluster Setup

## Set up GKE cluster with FilestoreCSI enabled

This guide uses a single-node zonal cluster for demonstration, though actual deployments will generally involve many nodes. Both regional and zonal cluster configurations are supported. Note that **the minimum supported GKE cluster version is 1.36.0-gke.3302001**. This is the minimum version that supports the gVisor annotation (`dev.gvisor.empty-dir.<name>.force-shared: "true"`), which is required to utilize an `emptyDir` mount path for dynamic late-binding in this guide.

```shell
# Set environment variables
export PROJECT_ID=$(gcloud config get project)
export CLUSTER_NAME="agent-sandbox-cluster"
export LOCATION="us-central1"
export CLUSTER_VERSION="1.36.0-gke.3302001"
export NODE_POOL_NAME="agent-sandbox-pool"
# Upgraded from default e2-medium to ensure sufficient resource headroom for multiple agent pods. While n2-standard-32 is used in this guide, the optimal machine size depends on your specific agent resource requirements and target pod density per node.
export MACHINE_TYPE="n2-standard-32"

# 1. Create the Cluster with FilestoreCSI Addon
gcloud container clusters create ${CLUSTER_NAME} \
--location=${LOCATION}-a \
--cluster-version=${CLUSTER_VERSION} \
--num-nodes=1 \
--machine-type=${MACHINE_TYPE} \
--addons=GcpFilestoreCsiDriver

# 2. Create the gVisor-enabled Node Pool
gcloud container node-pools create ${NODE_POOL_NAME} \
--cluster=${CLUSTER_NAME} \
--location=${LOCATION}-a \
--machine-type=${MACHINE_TYPE} \
--num-nodes=1 \
--image-type=cos_containerd \
--sandbox=type=gvisor
```

## Install Agent Sandbox

Choose one of the following installation methods.

### Standard OSS Installation 

To install the standard Open Source Agent Sandbox controller and CRDs, apply the base manifests followed by the extensions for warm pools:

```shell
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox.yaml
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/extensions.yaml
```

**Note on API Versions**: The OSS controller serves the `v1beta1` API. If you use this installation, you must update the apiVersion in the YAML examples throughout this guide from `extensions.agents.x-k8s.io/v1alpha1` to `extensions.agents.x-k8s.io/v1beta1`.

### GKE Agent Sandbox Add-on (Cloud-Specific Alternative)

Alternatively, you can enable the managed GKE Agent Sandbox add-on:

```shell
gcloud beta container clusters update ${CLUSTER_NAME} \
--location=${LOCATION}-a \
--enable-agent-sandbox
```

**Note**: The GKE add-on currently serves the `v1alpha1` API. The examples in this guide use `v1alpha1` for compatibility with this managed service.


# Infrastructure Deployment

## Deploy Storage Node Daemon Infrastructure

This configuration provisions the shared Filestore `PersistentVolumeClaim` and deploys a privileged node daemon running a local HTTP server. In this guide, you will manually manage host-level bind mounts by executing commands directly against this daemon pod. This walkthrough assumes the node daemon is deployed in the `default` namespace. However, as a best practice, it should be deployed in a separate namespace from your agent pods. If you move it to another namespace, define a `DAEMON_NAMESPACE` variable and add `-n $DAEMON_NAMESPACE` to every `kubectl get` / `kubectl exec` command that targets `storage-node-daemon`. If you change the namespace from default, also ensure you update the `namespace:` field in the YAMLs below. 

**Production Automation Note:** In this guide, the HTTP server binds strictly to localhost because we trigger the bind and unbind requests locally inside the daemon container's namespace via kubectl exec. When transitioning to a production environment, you will need to configure the daemon's networking and host access so that your external platform orchestrator or custom controller can securely reach the daemon's API to manage mounts.

Examine [`storage-infra.yaml`](storage-infra.yaml) for the Node Daemon infrastructure.

## Deploy SandboxTemplate and Warmpool

The `SandboxTemplate` defines the warm sandbox properties. The container's `/workspace` is mapped to an `emptyDir` volume using `HostToContainer` mount propagation, ensuring it receives directory mounts made on the host. 

The minimum supported GKE version for this implementation is 1.36.0-gke.3302001. Starting with this version, you can utilize the gVisor annotation `dev.gvisor.empty-dir.<volume-mount-name>.force-shared: "true"` to configure `emptyDir` mount paths that can be used for subdirectory bind mounts after the pod is running. 

Examine [`sandbox-warmpool.yaml`](sandbox-warmpool.yaml) for the `SandboxTemplate` and `SandboxWarmPool`. 

## Applying YAML Configurations

Apply all configurations to your GKE cluster:

```shell
kubectl apply -f storage-infra.yaml
kubectl apply -f sandbox-warmpool.yaml
```

Verify that both the warmpool sandboxes and the storage node daemon are up and running:

```shell
kubectl get pods
```

Expected Output 

```shell
late-bind-warmpool-b8g4w  2/2   Running  0     37s
late-bind-warmpool-bvvv6  2/2   Running  0     37s
storage-node-daemon-9zq5j  1/1   Running  0     8m59s
```

## Verify Initial Isolation and Pre-warmed Directory State

Let's look at the container and host-level directories of an unclaimed warm sandbox. The directory structure will exist but remain completely empty.

Set your target warm sandbox and the node daemon as variables:

```shell
export POD_NAME=late-bind-warmpool-b8g4w
export POD_UID=$(kubectl get pod $POD_NAME -o jsonpath='{.metadata.uid}')
export NODE_NAME=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
export DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NODE_NAME -o jsonpath='{.items[0].metadata.name}')
```

### 1. Check Directory State from the Node Host (Daemon perspective)

Verify the empty directory configuration inside `/host-root/var/lib/kubelet/pods/`:

```shell
kubectl exec $DAEMON_POD -c daemon -- ls -laR /host-root/var/lib/kubelet/pods/$POD_UID/volumes/kubernetes.io~empty-dir/workspace-volume/
```

Expected Output 

```shell
/host-root/var/lib/kubelet/pods/3ce30fe8-87f8-4854-aba5-3420725a5d4d/volumes/kubernetes.io~empty-dir/workspace-volume/:
total 8
drwxrwsrwx 2 root 1000 4096 Jun 14 22:14 .
drwxr-xr-x 5 root root 4096 Jun 14 22:14 ..
```

### 2. Check Directory State from the Agent Container

Check the mapped volume inside the running agent:

```shell
kubectl exec $POD_NAME -c agent -- ls -lRa /workspace/
```

Expected Output

```shell
total 4
drwxrwsrwx 2 root 1000 4096 Jun 14 22:14 .
drwxr-xr-x 1 root root  60 Jun 14 22:14 ..
```

*Notice that* *`/workspace`* *is empty, and the agent container command is waiting for the late-bind signal (a file named `.ready`, will be explained later).*

# Basic Operations: Claiming & Dynamic Storage Binding

## Submit a Sandbox Claim

To allocate a pre-warmed container to a specific tenant (e.g., `user-111`), submit a claim.

Apply the following `SandboxClaim`:

```shell
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
 name: user-111-claim
 finalizers:
  - agent.sandbox/storage-cleanup
spec:
 sandboxTemplateRef:
  name: late-bind-template
EOF
```

Output:

```shell
sandboxclaim.extensions.agents.x-k8s.io/user-111-claim created
```

### Why we use Finalizers

Because our custom node daemon performs host-level bind mounts outside of standard Kubernetes volume control, Kubernetes is unaware of this active attachment. This creates a critical lifecycle challenge during deletions:
- **The Volume Busy Error**: If an active agent pod is deleted—either manually by deleting the Pod or SandboxClaim bound to a Pod or during automatic cluster maintenance like node draining or evictions (such as node upgrades, cluster autoscaler scale-down, or spot VM evictions)—Kubelet will immediately attempt to clean up the pod's emptyDir folder. Since the directory is still actively bind-mounted, Kubelet's unmount operation fails with a "busy" directory error.
- **The Rescheduling Block**: This leaves the old pod permanently stuck in an Error, which blocks the SandboxClaim from ever successfully binding to a fresh, healthy pod.
- **The Finalizer Solution**: To block premature deletions, we use an `agent.sandbox/storage-cleanup` finalizer on the SandboxClaim resource. The finalizer acts as a mandatory deletion block, forcing the Kubernetes API to pause the destruction process. This gives your orchestrator the necessary window to safely trigger the unbind API call via the Node Daemon before clearing the finalizers.

**Production Automation Note:** In this guide, we declare and manage the `agent.sandbox/storage-cleanup` finalizers manually using CLI commands for demonstration purposes. In a production environment, you should write a custom controller to automate this entire process to safely unbind the storage before allowing GKE to cleanly delete the pod and claim.

## Capture Sandbox and Node Metadata

Obtain the specific instance metadata of the claimed pod. 

```shell
export POD_NAME=$(kubectl get sandboxclaim user-111-claim -o jsonpath='{.status.sandbox.name}')
export POD_UID=$(kubectl get pod $POD_NAME -o jsonpath='{.metadata.uid}')
export NODE_NAME=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
export DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NODE_NAME -o jsonpath='{.items[0].metadata.name}')
```

Verify your captured variables:

```shell
echo "Claimed Pod: $POD_NAME | UID: $POD_UID | Node Name: $NODE_NAME | Daemon Pod: $DAEMON_POD"
```

Expected Output 

```shell
Claimed Pod: late-bind-warmpool-b8g4w | UID: 3ce30fe8-87f8-4854-aba5-3420725a5d4d | Node Name: gke-agent-sandbox-cl-agent-sandbox-po-86c6903c-5s9b | Daemon Pod: storage-node-daemon-9zq5j
```

At this stage, checking the logs of the claimed container shows it is still waiting:

```shell
kubectl logs $POD_NAME -c agent
```

Expected Output 

```shell
Waiting for local late-bind signal in emptyDir root...
```

**Production Automation Note**: In a production environment, this would be automated by an orchestrator/controller that actively watches the Kubernetes API for the `SandboxClaim` status. Once GKE binds the claim to a pod, the orchestrator programmatically extracts the assigned Pod's UID and host IP to route the subsequent bind request to the correct Node Daemon.

## Trigger Dynamic Storage Binding (HTTP Request)

Using Python's `urllib.request` library, the Workload team manually triggers an `HTTP POST` request directly against the node daemon container to send a late-bind request.

### Triggering the Host Bind Mount

This command instructs the node daemon to perform the host-level subdirectory bind-mount, recursively adjust target directory ownership to UID 1000 (allowing container write access inside the sandbox), and write a `.ready` signal file directly to the parent emptyDir on the host to unblock the agent's execution gate.

**Agent Access Mode Adaptation**: By varying the request payload here, you can switch access modes based on your agent's lifecycle needs:
- Private Isolated Workspaces: Map a specific `user_id` to a pod for exclusive data access.
- Collaborative Workspaces: Send the same persistent path to multiple pods to allow real-time shared file updates.
- Exploration Branching: Send the `is_readonly: true` flag to mount an immutable base template. This allows the agent to read from the master copy while protecting it from corruption, as all new writes are stored in the pod's local writable layer. If your agent logic requires writing directly into the same path structure as the template (a "writable overlay"), you would need to implement a more complex union filesystem or have the agent copy the specific files it needs to modify into the root `/workspace` directory at startup.
Execute the following commands to bind the storage:

```shell
kubectl exec $DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'bind', \
  'user_id': 'user-111', \
  'pod_uid': '$POD_UID', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"
```

Expected Output 

```shell
Success: Bound Filestore folder user-111 to mount path inside of agent container
```

### Securing the Running Pod with a Dynamic Finalizer

While we statically declared a finalizer on the SandboxClaim in [Submit a SandboxClaim](#submit-a-sandbox-claim), we must also place a finalizer on the `SandboxWarmPool` Pods that are bound to `SandboxClaim`s. [Kubernetes does not support](https://github.com/kubernetes-sigs/agent-sandbox/blob/aae8e6272688daaf45a4a890f813da29aa73de7e/api/v1alpha1/sandbox_types.go#L124-L127) defining static finalizers inside a `SandboxTemplate`'s `podTemplate.spec` block. Therefore, the orchestrator must dynamically apply the finalizer to the running Pod immediately after the storage is successfully bound. This establishes a secure 1:1 mapping between the bound storage and the pod lifecycle, ensuring that the pod cannot be deleted until the host-level mount is safely decoupled.

Execute the following commands to dynamically patch the pod-level finalizer:

```shell
# Apply the deletion finalizer directly to the Pod
# This prevents GKE from deleting the pod object before we execute the unbind command.
kubectl patch pod $POD_NAME --type=merge -p '{"metadata":{"finalizers":["agent.sandbox/storage-cleanup"]}}'
```

**Production Automation Note**: In a production environment, a custom controller must be built to automate this entire sequence rather than managing finalizers and mounts manually through the CLI. This controller should actively watch GKE's API for SandboxClaim bindings to capture pod metadata, programmatically trigger the late-bind HTTP request directly to the target node's daemon, and atomically apply the pod-level finalizer metadata patch. Finally, during session termination or unexpected evictions, the controller must intercept the deletion, safely execute the daemon-level unbind, and cleanly strip away the finalizers only after the unmount has fully completed to prevent stuck pods and resource leaks.


## Validate Mount Success


Once the node daemon performs the host bind mount, the directory structure gets exposed to the agent container instantly.

### 1. Check Files Inside the Agent Container

Check `/workspace` to verify that `.ready` was created by the daemon, and that the container's shell script broke out of its wait loop and generated a `state.txt` session log:

```shell
kubectl exec $POD_NAME -c agent -- ls -Rla /workspace
```

Expected Output

```shell
total 5
drwxrwsrwx 3 root 1000 4096 Jun 14 22:18 .
drwxr-xr-x 1 root root  60 Jun 14 22:14 ..
-rw-r--r-- 1 root 1000  5 Jun 14 22:18 .ready
drwxr-xr-x 2 1000 1000  0 Jun 14 22:18 user_data
/workspace/user_data:
total 5
drwxr-xr-x 2 1000 1000  0 Jun 14 22:18 .
drwxrwsrwx 3 root 1000 4096 Jun 14 22:18 ..
-rw-r--r-- 1 1000 1000  72 Jun 14 22:18 state.txt
```

### 2. Verify Container State logs

Verify the initialization of the agent session:

```shell
kubectl logs $POD_NAME -c agent
```

Expected Output

```shell
Waiting for local late-bind signal in emptyDir root...
Data bound! Resilience verified.
New Session Initialized.
Starting custom agent application...
```

### 3. Check Host-Level Mount Integrity (Daemon perspective)

Verify the actual bind mount on the node file system under `/var/lib/kubelet` from the node daemon container:

```shell
kubectl exec $DAEMON_POD -c daemon -- ls -laR /host-root/var/lib/kubelet/pods/$POD_UID/volumes/kubernetes.io~empty-dir/workspace-volume/
```

Expected Output 

```shell
/host-root/var/lib/kubelet/pods/3ce30fe8-87f8-4854-aba5-3420725a5d4d/volumes/kubernetes.io~empty-dir/workspace-volume/:
total 12
drwxrwsrwx 3 root 1000 4096 Jun 14 22:18 .
drwxr-xr-x 5 root root 4096 Jun 14 22:14 ..
-rw-r--r-- 1 root 1000  5 Jun 14 22:18 .ready
drwxr-xr-x 2 1000 1000  0 Jun 14 22:18 user_data
/host-root/var/lib/kubelet/pods/3ce30fe8-87f8-4854-aba5-3420725a5d4d/volumes/kubernetes.io~empty-dir/workspace-volume/user_data:
total 8
drwxr-xr-x 2 1000 1000  0 Jun 14 22:18 .
drwxrwsrwx 3 root 1000 4096 Jun 14 22:18 ..
-rw-r--r-- 1 1000 1000  72 Jun 14 22:18 state.txt
```

The dynamic Filestore dynamic late-binding execution is complete and isolation is verified!

## Pausing and Resuming an Existing Session

Since Filestore stores directories persistently, the late-bind mechanism preserves files across pod lifecycles. If a tenant's container is destroyed or recycled, their directory can be late-bound to a completely new `SandboxWarmpool` Pod, which is bound to a different `SandboxClaim`, restoring their session workspace instantly.

### Preventing Resource Leaks During Session Teardown

As established in [Submit a SandboxClaim](#submit-a-sandbox-claim) and [Trigger Dynamic Storage Binding (HTTP Request)](#trigger-dynamic-storage-binding-http-request), because host-level bind mounts bypass standard Kubernetes CSI controls, the finalizers on both the SandboxClaim and the Pod act as mandatory deletion blocks. This design ensures your orchestrator has a secure window to safely execute the unbind workflow during termination, regardless of whether the deletion is planned or unexpected:
- **Graceful Terminations (SandboxClaim Deletion):** Deleting the `SandboxClaim` (manually or via system triggers) schedules both the claim and its bound pod for deletion. The finalizers pause the API destruction process, keeping the pod's emptyDir mount intact so the orchestrator can call unbind via the Node Daemon before the container and its resources are terminated.
- **Pod Evictions & Rescheduling (Node Drains / Upgrades):** During automatic GKE maintenance events like node draining or evictions (such as node upgrades, cluster autoscaler scale down, or spot VM evictions), the SandboxClaims themselves are not deleted; only the underlying pods are rescheduled to new nodes. In these scenarios, the pod-level finalizer is critical: it holds the Pod in a Terminating state, preventing GKE from deleting the pod object before an unbind occurs, which avoids broken volume busy loops and ensures GKE can safely spin up a healthy replacement pod once cleared.
- **Unexpected Shutdowns (OOM Kills, Node Crashes):** During OOMs / node crashes, the finalizer keeps the pod object alive in the API (even if its containers are dead), allowing your custom controller to safely trigger unbind and prevent orphaned host-level mount points on the host VM before removing the finalizer to let GKE cleanly destroy the pod.
**Production Automation Note**:  In this guide, you will manually execute the unbind request via the node daemon and remove the finalizers when terminating a session. In a production environment, a custom controller or session orchestrator must automate this entire lifecycle. This controller should actively watch GKE's API for both `SandboxClaim` deletions and individual Pod deletion timestamps or rescheduling events, trigger the /unbind HTTP API call, and remove the finalizers only after unmounting has completed to prevent resource leaks and stuck pods.

### Pausing / Resuming a Session

Let's simulate this by destroying the claim for `user-111`, creating a new claim, and re-binding the original `user-111` workspace.

#### 1. Delete user-111's Old Claim

Because the claim has a finalizer attached, the Kubernetes API acts as a deletion block, pausing the actual destruction. The `kubectl delete` command will hang and block your terminal (which is why we append & to run it in the background) to give the orchestrator time to safely execute the unbind command before the pod is destroyed.

```shell
kubectl delete sandboxclaim user-111-claim &
```

#### 2. Orchestrator Performs Safe Unbind

The orchestrator sends an HTTP request to the local daemon, which cleans up the directory state by safely unmounting the host path without destroying the underlying files.

```shell
# Re-export variables just in-case.
export POD_NAME=$(kubectl get sandboxclaim user-111-claim -o jsonpath='{.status.sandbox.name}')
export POD_UID=$(kubectl get pod $POD_NAME -o jsonpath='{.metadata.uid}')
export NODE_NAME=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
export DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NODE_NAME -o jsonpath='{.items[0].metadata.name}')

# Perform the unbind command
kubectl exec $DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'unbind', \
  'user_id': 'user-111', \
  'pod_uid': '$POD_UID', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"
```

#### 3. Orchestrator Removes Finalizer (Pod safely terminates)

Once the directory is safely unmounted, the orchestrator manually removes the finalizers from both the SandboxClaim and the Pod. This allows GKE to safely clean up the unmounted emptyDir and fully destroy the pod.

```shell
kubectl patch sandboxclaim user-111-claim --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch pod $POD_NAME --type=merge -p '{"metadata":{"finalizers":[]}}'
```

#### 4. Create a Brand New Claim for user-111

To resume the session, a new claim is created, which automatically assigns a fresh pre-warmed pod from the background warm pool.
```shell
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
 name: user-111-resume
 finalizers:
  - agent.sandbox/storage-cleanup
spec:
 sandboxTemplateRef:
  name: late-bind-template
EOF
```

#### 5. Capture the New Pod Details

```shell
export NEW_POD_NAME=$(kubectl get sandboxclaim user-111-resume -o jsonpath='{.status.sandbox.name}')
export NEW_POD_UID=$(kubectl get pod $NEW_POD_NAME -o jsonpath='{.metadata.uid}')
export NEW_NODE_NAME=$(kubectl get pod $NEW_POD_NAME -o jsonpath='{.spec.nodeName}')
export NEW_DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NEW_NODE_NAME -o jsonpath='{.items[0].metadata.name}')
```

Verify that the new pod is waiting for a bind-mount:

```shell
kubectl logs $NEW_POD_NAME -c agent
```

Expected Output 

```shell
Waiting for local late-bind signal in emptyDir root...
```

#### 6. Bind the Existing user-111 Directory to the New Pod

Now, make the HTTP request to bind the same directory (`user-111`) to this new container's pod UID. Crucially, we must also apply the dynamic finalizer to the new pod to protect it from [resource leaks during session teardown](#preventing-resource-leaks-during-session-teardown)

```shell
# Trigger the Node Daemon to bind-mount the directory
kubectl exec $NEW_DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'bind', \
  'user_id': 'user-111', \
  'pod_uid': '$NEW_POD_UID', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"

# Lock the new pod with the deletion finalizer
kubectl patch pod $NEW_POD_NAME --type=merge -p '{"metadata":{"finalizers":["agent.sandbox/storage-cleanup"]}}'
```

Expected Output:

```shell
Success: Bound Filestore folder user-111 to mount path inside of agent container
```

#### 7. Verify Session Resumption Logs

Look at the new agent's startup logs. Because `state.txt` existed from the previous session, the script printed the previous session history and appended a new heartbeat 

```shell
kubectl logs $NEW_POD_NAME -c agent
```

Expected Output:

```shell
Waiting for local late-bind signal in emptyDir root...
Data bound! Resilience verified.
Heartbeat from late-bind-warmpool-b8g4w at Sun Jun 14 22:18:09 UTC 2026
Starting custom agent application...
```

Verify that `state.txt` now contains *both* heartbeat timestamps (showing both the old and new pod interactions) 

```shell
kubectl exec $NEW_POD_NAME -c agent -- cat /workspace/user_data/state.txt
```

Expected Output:

```shell
Heartbeat from late-bind-warmpool-b8g4w at Sun Jun 14 22:18:09 UTC 2026
Heartbeat from late-bind-warmpool-bvvv6 at Sun Jun 14 22:20:14 UTC 2026
```

Session resumption is fully operational! The user's historical data was re-attached to the new warmpool instance dynamically, preserving the exact state of the persistent workspace.

# Advanced Scenarios

## Point-in-Time Snapshot and Restore

To achieve sub-second point-in-time restore, we archive an active workspace into a snapshot subdirectory on the shared Filestore volume and then instruct the node daemon to bind that specific historical version to a new warm pod. 

### Archive a Point-in-Time Snapshot

In this step, we will create a historical snapshot using the persistent data from `$NEW_POD_NAME` that was active in the previous session. To ensure data integrity, the orchestrator must achieve **quiescence**—confirming that all active writes have stopped—before triggering a local file-level copy. This prevents the creation of corrupted or inconsistent snapshots.

In this workflow, quiescence is achieved by deleting the `SandboxClaim`. This initiates the pod deletion process but pauses due to the finalizer, allowing the orchestrator to flush all writes and unmount the directory safely before the pod is actually gone. Once the data remains persistently on the Filestore volume, it is ready to be archived. 

**Production Automation Note**: To handle snapshot and restores at scale, orchestrators should run their own custom controller or operator integrated with their session management logic. This automated logic would typically follow this sequence:
- Monitor Termination: The controller watches the GKE API for `SandboxClaim` deletion events.
- Verify Quiescence: It executes the safe unmount via the Node Daemon and removes the finalizers so the pod can terminate.
- Trigger Archival: Once the pod is gone and the data is consistent, the controller automatically captures the metadata (Node name, User ID) and issues the HTTP POST request to the local `storage-node-daemon` to execute the snapshot copy.
- Metadata Registration: The controller then updates an external metadata database, pairing the new `snapshot_id` with timestamps or version labels for future restorations.
```shell
# Delete the claim to stop active agent writes
# The finalizer pauses deletion, allowing the orchestrator to perform a safe unbind.
kubectl delete sandboxclaim user-111-resume &

export NEW_POD_NAME=$(kubectl get sandboxclaim user-111-resume -o jsonpath='{.status.sandbox.name}')
export NEW_POD_UID=$(kubectl get pod $NEW_POD_NAME -o jsonpath='{.metadata.uid}')
export NEW_NODE_NAME=$(kubectl get pod $NEW_POD_NAME -o jsonpath='{.spec.nodeName}')
export NEW_DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NEW_NODE_NAME -o jsonpath='{.items[0].metadata.name}')


# Orchestrator Performs Safe Unbind to ensure data is written and cleanly decoupled
kubectl exec $NEW_DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({'action': 'unbind', 'user_id': 'user-111', 'pod_uid': '$NEW_POD_UID', 'volume_name': 'workspace-volume', 'sub_dir': 'user_data'}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"

# Remove the Finalizer from both the SandboxClaim and the Pod to allow GKE to cleanly terminate the pod
kubectl patch sandboxclaim user-111-resume --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch pod $NEW_POD_NAME --type=merge -p '{"metadata":{"finalizers":[]}}'


# Define a unique snapshot identifier
export SNAPSHOT_ID="user-111-v1-snapshot"

# Execute the Archive Commands via the Storage Node Daemon
# 'mkdir' ensures the snapshots directory exists on the shared volume.
kubectl exec $NEW_DAEMON_POD -c daemon -- mkdir -p /mnt/master-volume/snapshots/

# 'cp -rp' performs a recursive copy that preserves physical permissions and timestamps.
# This "freezes" the current state of user-111 into a dedicated historical subdirectory.
kubectl exec $NEW_DAEMON_POD -c daemon -- cp -rp /mnt/master-volume/users/user-111 /mnt/master-volume/snapshots/$SNAPSHOT_ID

echo "Quiescence verified and snapshot $SNAPSHOT_ID archived."
```

### Restore from Snapshot (Claiming and Binding)

When the user requests a restoration, the orchestrator issues a new claim and instructs the node daemon to bind-mount the specific snapshot subdirectory instead of the default active user directory.

**Production Automation Note**: In this guide, the workload team manually triggers the restoration binding by sending an `HTTP POST` request. In a production environment, your custom orchestrator/controller must automate sending the restoration bind request (with the `snapshot_id`) to the target node's daemon.

**Agent Access Mode Adaptation**: The orchestrator implements different access modes by varying the payload in the `HTTP POST` request to the node daemon:
- Private Isolated Workspaces: The orchestrator performs a "Copy-on-Restore" by duplicating the historical snapshot to a new writable path before binding, ensuring the agent can modify the state without affecting the original snapshot.
- Collaborative Workspaces: The orchestrator triggers identical POST requests for multiple coordinating agents, binding them concurrently to the same restored ReadWriteMany (RWX) volume subdirectory.
- Exploration Branching Workspaces: The orchestrator includes the `is_readonly: true` flag in the POST request. The daemon mounts the snapshot as Read-Only, forcing the agent to redirect all new modifications to a separate local scratchpad while preserving the immutable master copy. If the agent logic requires writing directly into the same path structure as the template (a "writable overlay"), you would need to implement a more complex union filesystem or have the agent copy the specific files it needs to modify into the root `/workspace` directory at startup

```shell
# 1. Create a new SandboxClaim for the restoration session
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
 name: user-111-snapshot-restore
 finalizers:
  - agent.sandbox/storage-cleanup
spec:
 sandboxTemplateRef:
  name: late-bind-template
EOF

# 2. Capture metadata for the newly assigned warm pod
export RESTORE_POD_NAME=$(kubectl get sandboxclaim user-111-snapshot-restore -o jsonpath='{.status.sandbox.name}')
export RESTORE_POD_UID=$(kubectl get pod $RESTORE_POD_NAME -o jsonpath='{.metadata.uid}')
export RESTORE_NODE_NAME=$(kubectl get pod $RESTORE_POD_NAME -o jsonpath='{.spec.nodeName}')
export RESTORE_DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$RESTORE_NODE_NAME -o jsonpath='{.items[0].metadata.name}')

# 3. Trigger the late-bind request with the 'snapshot_id' parameter
# The daemon script is programmed to prioritize snapshot_id if provided.
# The orchestrator can optionally add "is_readonly": true for Golden Copy restoration
kubectl exec $RESTORE_DAEMON_POD -c daemon -- python3 -c "import urllib.request, json, os; \
data = json.dumps({ \
  'action': 'bind', \
  'user_id': 'user-111', \
  'snapshot_id': '$SNAPSHOT_ID', \
  'pod_uid': '$RESTORE_POD_UID', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data', \
  'is_readonly': False \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"

# Apply the deletion finalizer to the restored pod to prevent unmount issues
kubectl patch pod $RESTORE_POD_NAME --type=merge -p '{"metadata":{"finalizers":["agent.sandbox/storage-cleanup"]}}'
```

### Verify Snapshot Integrity & Restoration

The agent container detects the `.ready` file, breaks its wait loop, and accesses the historical state archived in the snapshot 

```shell
kubectl logs $RESTORE_POD_NAME -c agent
```

**Expected Output:** The logs will show the heartbeats from the previous pod sessions, proving that the historical state was preserved and restored instantly without network file transfers 

```shell
Waiting for local late-bind signal in emptyDir root...
Data bound! Resilience verified.
Heartbeat from late-bind-warmpool-b8g4w at Sun Jun 14 22:18:09 UTC 2026
Heartbeat from late-bind-warmpool-bvvv6 at Sun Jun 14 22:20:14 UTC 2026
Starting custom agent application...
```

## Multi-Tenant Data Isolation

This step demonstrates that even when multiple tenants are active on the same Filestore volume, gVisor and the bind-mount mechanism ensure they cannot access each other's data 

### 1. Create the SandboxClaim for user-222
```shell
kubectl apply -f - <<EOF
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
 name: user-222-claim
 finalizers:
  - agent.sandbox/storage-cleanup
spec:
 sandboxTemplateRef:
  name: late-bind-template
EOF
```

### 2. Send Bind Request for user-222
```shell
export POD_NAME_2=$(kubectl get sandboxclaim user-222-claim -o jsonpath='{.status.sandbox.name}')
export POD_UID_2=$(kubectl get pod $POD_NAME_2 -o jsonpath='{.metadata.uid}')
export NODE_NAME_2=$(kubectl get pod $POD_NAME_2 -o jsonpath='{.spec.nodeName}')
export DAEMON_POD_2=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NODE_NAME_2 -o jsonpath='{.items[0].metadata.name}')
# Trigger the Node Daemon to bind the persistent directory for user-222
kubectl exec $DAEMON_POD_2 -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({'action': 'bind', 'user_id': 'user-222', 'pod_uid': '$POD_UID_2', 'volume_name': 'workspace-volume', 'sub_dir': 'user_data'}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"
# Patch the second pod to protect it from premature deletion
kubectl patch pod $POD_NAME_2 --type=merge -p '{"metadata":{"finalizers":["agent.sandbox/storage-cleanup"]}}'
```

### 3. Verify Isolation

Let's create some custom content in `user-222`'s storage space to verify attempting to list `/workspace/user_data` in `user-222`'s pod will only show their own files, and they will be physically blocked from accessing the directory of `user-111` through the host filesystem boundaries.

Create custom content in `user-222`'s storage space. Drop a specific marker file in the workspace of `user-222` to serve as a target for the visibility check.

```shell
kubectl exec $POD_NAME_2 -c agent -- sh -c "echo 'Secret User 222 Data' > /workspace/user_data/secret_222.txt"
```

Verify Isolation Between `user-111` and `user-222`. Inspect the workspace of `user-111` to confirm they cannot see the secret file created by `user-222`.

Check `user-111`'s container directory:
```shell
kubectl exec $RESTORE_POD_NAME -c agent -- ls -la /workspace/user_data/
```
- Expected Output (Only Tenant 1's files exist): The output will show `state.txt` (the restored heartbeat log), but `secret_222.txt` will be missing.

Check `user-222`'s container directory:
```shell
kubectl exec $POD_NAME_2 -c agent -- ls -la /workspace/user_data/
```
- Expected Output (Only Tenant 2's files exist): The output will show `secret_222.txt` and a fresh `state.txt`, but it will *not* contain the historical heartbeat data belonging to `user-111`.

## Cleanup Tenant's Persistent Directory

When a user’s persistent state is no longer required, the orchestrator can trigger a final cleanup to release storage space on the shared Filestore volume. This action is destructive and will permanently remove all files within the user's persistent directory. Before permanently deleting a user's persistent directory from the shared Filestore volume, you must unmap any active agent pods using the `unbind` action. This prevents pods from writing to directories during deletion, avoiding write errors or data corruption. Unmapping involves calling unbind, deleting the SandboxClaim/Pods, and clearing finalizers—steps omitted here as they are covered in detail in the final teardown sections of this guide.

**Production Automation Note**: In this guide, the cleanup is shown as a manual task performed by the workload team, but in a production environment, a custom orchestrator/controller must automate the permanent deletion of persistent directories once a user's state is no longer required.

### 1. Trigger the Cleanup Request

The orchestrator sends an `HTTP POST` request to the node daemon with the delete action. To delete a specific historical snapshot instead of an active user directory, simply include the `snapshot_id` in the request payload. The daemon is programmed to prioritize the snapshot path if a `snapshot_id` is provided.

```shell
# Pick any running storage-node-daemon pod; the delete action only targets Filestore paths.
export RESTORE_DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon -o jsonpath='{.items[0].metadata.name}')

# Trigger the daemon to delete the persistent storage for user-111
kubectl exec $RESTORE_DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'delete', \
  'user_id': 'user-111', \
  'pod_uid': 'unused', \
  'volume_name': 'unused', \
  'sub_dir': 'unused' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"
```

### 2. Verify Deletion

Confirm that the directory has been removed from the master Filestore volume:
```shell
kubectl exec $RESTORE_DAEMON_POD -c daemon -- ls -la /mnt/master-volume/users/
```

Expected Output: The `user-111` directory should no longer appear in the listing.

```shell
drwxr-xr-x 3 root root 0 Jun 22 20:21 .
drwxr-xr-x 4 root root 0 Jun 22 20:19 ..
drwxr-xr-x 2 1000 1000 0 Jun 22 20:21 user-222
```

# Demo Infrastructure Teardown & Resource Cleanup

## Complete Clean up of Demo Resources and GKE Cluster

This final section completely cleanses the GKE environment of all objects, controllers, PVCs, and clusters provisioned during the demo, preventing cloud charges and ensuring zero resource leakage.

### 1. Safely Unbind and Clean up Active SandboxClaims

Since active SandboxClaims and Pods have finalizers protecting them, we must execute the unbind command, and strip finalizers from any active claims/pods before Kubernetes will allow them to be deleted.

```shell
# Reexport variables just in-case.
export RESTORE_POD_NAME=$(kubectl get sandboxclaim user-111-snapshot-restore -o jsonpath='{.status.sandbox.name}')
export RESTORE_POD_UID=$(kubectl get pod $RESTORE_POD_NAME -o jsonpath='{.metadata.uid}')
export RESTORE_NODE_NAME=$(kubectl get pod $RESTORE_POD_NAME -o jsonpath='{.spec.nodeName}')
export RESTORE_DAEMON_POD=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$RESTORE_NODE_NAME -o jsonpath='{.items[0].metadata.name}')
export POD_NAME_2=$(kubectl get sandboxclaim user-222-claim -o jsonpath='{.status.sandbox.name}')
export POD_UID_2=$(kubectl get pod $POD_NAME_2 -o jsonpath='{.metadata.uid}')
export NODE_NAME_2=$(kubectl get pod $POD_NAME_2 -o jsonpath='{.spec.nodeName}')
export DAEMON_POD_2=$(kubectl get pods -l app=storage-node-daemon --field-selector spec.nodeName=$NODE_NAME_2 -o jsonpath='{.items[0].metadata.name}')

# Perform the unbind commands
kubectl exec $DAEMON_POD_2 -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'unbind', \
  'user_id': 'user-222', \
  'pod_uid': '$POD_UID_2', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"

kubectl exec $RESTORE_DAEMON_POD -c daemon -- python3 -c "import urllib.request, json; \
data = json.dumps({ \
  'action': 'unbind', \
  'user_id': 'user-111-snapshot-restore', \
  'pod_uid': '$RESTORE_POD_UID', \
  'volume_name': 'workspace-volume', \
  'sub_dir': 'user_data' \
}).encode('utf-8'); \
req = urllib.request.Request('http://localhost:9090/', data=data, headers={'Content-Type': 'application/json'}); \
print(urllib.request.urlopen(req).read().decode())"

# Delete remaining SandboxClaims
kubectl delete sandboxclaim user-111-snapshot-restore user-222-claim --ignore-not-found=true

# Strip finalizers from active claims to allow graceful deletion
kubectl patch sandboxclaim user-111-snapshot-restore --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch sandboxclaim user-222-claim --type=merge -p '{"metadata":{"finalizers":[]}}'

# Strip finalizers from any remaining agent-sandbox pods 
kubectl patch pod $RESTORE_POD_NAME --type=merge -p '{"metadata":{"finalizers":[]}}'
kubectl patch pod $POD_NAME_2 --type=merge -p '{"metadata":{"finalizers":[]}}'
```

### 2. Terminate SandboxWarmPool and SandboxTemplate

```shell
kubectl delete -f sandbox-warmpool.yaml
```

### 3. Terminate Node Daemon and Filestore PVC

This command tears down the DaemonSet hosting the Storage Node Daemon as well as the 1Ti Filestore Regional RWX volume claim:

```shell
kubectl delete -f storage-infra.yaml
```


### 4. Delete the GKE Cluster and Nodes
If you no longer need the test cluster, delete the entire GKE cluster to completely avoid ongoing cloud infrastructure costs:

```shell
export CLUSTER_NAME="agent-sandbox-cluster"
export LOCATION="us-central1"

gcloud container clusters delete ${CLUSTER_NAME} \
--location=${LOCATION}-a
```

