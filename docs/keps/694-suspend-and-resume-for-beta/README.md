# KEP-0694: Suspend and Resume + Snapshot Provider API for Agent Sandboxes Beta

<!-- toc -->
- [Motivation](#motivation)
- [Use Cases](#use-cases)
- [User Personas](#user-personas)
- [Goals for Beta](#goals-for-beta)
- [Non-Goals for Beta](#non-goals-for-beta)
- [Phased Approach](#phased-approach)
  - [Phase 1: Beta](#phase-1-beta)
  - [Phase 2: Post-Beta / Future Additions](#phase-2-post-beta--future-additions)
- [API for Triggering Suspend/Resume in Beta](#api-for-triggering-suspendresume-in-beta)
  - [Alternatives Considered for Triggering Suspend/Resume](#alternatives-considered-for-triggering-suspendresume)
    - [1. API Aggregation Server (True Custom Subresources) - <em>Rejected</em>](#1-api-aggregation-server-true-custom-subresources---rejected)
    - [2. Ephemeral &quot;Action&quot; Custom Resources - <em>Rejected</em>](#2-ephemeral-action-custom-resources---rejected)
    - [3. The <code>spec.suspend</code> Boolean + <code>spec.strategy</code> - <em>Selected for Beta</em>](#3-the-specsuspend-boolean--specstrategy---selected-for-beta)
    - [4. The <code>spec.replicas</code> + <code>spec.strategy</code> (The /scale Pivot) - <em>Rejected</em>](#4-the-specreplicas--specstrategy-the-scale-pivot---rejected)
- [Suspension Strategies Explained](#suspension-strategies-explained)
- [API for Snapshotting: SnapshotClass vs SnapshotProvider](#api-for-snapshotting-snapshotclass-vs-snapshotprovider)
  - [SnapshotProvider vs SnapshotClass](#snapshotprovider-vs-snapshotclass)
  - [Architecture](#architecture)
  - [Who uses this CRD?](#who-uses-this-crd)
  - [API Design](#api-design)
  - [Suspension Options](#suspension-options)
  - [How will the conditions be exposed?](#how-will-the-conditions-be-exposed)
- [How Resume Works](#how-resume-works)
<!-- /toc -->

## Motivation

Long-running agents and development environments often exhibit bursty usage patterns, with periods of high activity followed by extended idle times (e.g., waiting for API responses, human input, or scheduled triggers). Currently, maintaining the execution context of these workloads requires keeping expensive compute resources active. While terminating pods via `replicas=0` and persisting disk state via Persistent Volumes is supported, the critical missing capability is capturing and restoring live memory state.

By introducing Suspend and Resume capabilities, we can take a memory and filesystem snapshot, scale resources down to zero, and quickly restore them when needed. This significantly reduces cloud spend while giving users the illusion of a permanently running agent. Additionally, this feature enables rapid scaling via "starter" templates, absolute control over infrastructure disruptions, and advanced parallel workflows.

## Use Cases

1. **The "Illusion of an Always-On" Agent (Efficiency during Idleness)**
   * **Scenario:** Agents operate in bursts followed by extended periods of inactivity.
   * **Need:** Take a memory and filesystem snapshot, scale resources down, and quickly restore them when needed to save idle costs without losing the execution context.

2. **Fast Launch via "Starter" Clones (Template Sandbox)**
   * **Scenario:** Launching new sandbox environments from scratch introduces significant cold-start latency.
   * **Need:** Spin up a "template" sandbox, install prerequisites, take a base snapshot, and suspend it. New agents can instantly "resume" from this starter snapshot instead of cold-booting.

3. **Absolute Disruption Control (Survival of Pod Evictions)**
   * **Scenario:** Kubernetes pods can be evicted or disrupted due to node maintenance, spot instance preemption, or autoscaling.
   * **Need:** Take regular snapshots or trigger emergency checkpoints prior to termination signals so the agent's execution can survive infrastructure disruptions and resume on a new node without losing progress.

4. **Advanced Workflows (Rewind & Fork)**
   * **Scenario:** An agent encounters a catastrophic error or takes an unwanted branch in code execution.
   * **Need:** Utilize snapshots to rewind the sandbox to a healthy checkpoint or fork an existing sandbox state into parallel environments to test different paths simultaneously.

## User Personas

| Customer Profile | How They Use It | Why Suspend/Resume Matters to Them |
| :--- | :--- | :--- |
| **SaaS Platform Builders** (Agent-as-a-Service / Dev Environments) | Integrating the API with custom runtimes (like gVisor or micro-VMs) via a pluggable `SnapshotProvider` model. | **Cost & Latency:** Support millions of user sandboxes by aggressively scaling-to-zero during idle time, and using cloned starter templates to eliminate cold-start times. |
| **Enterprise IT & Platform Engineers** | Operating Kubernetes clusters running untrusted LLM code, relying on automated scheduler policies. | **Resiliency & Resource Management:** Declarative "Auto-suspend" policies to clean up abandoned environments, snapshot retention/garbage collection, and scheduling controls (node affinity) upon resume. |
| **End-User Developers & Data Scientists** | Interacting with coding sandboxes (e.g., VSCode or Jupyter-based setups). | **Frictionless UX:** Persistent development environments that never lose terminal state, open file buffers, or running processes—even across weekends or node restarts. |

## Goals for Beta

* provide trigger for manual suspension and resumption of sandboxes.
* Provide a unified, pluggable API that abstracts runtime-specific snapshotting implementations (e.g., gVisor, Firecracker).
* Separate infrastructure configuration (Cluster Admin) from sandbox lifecycle requests (Developer/Agent).

## Non-Goals for Beta

* Implementing the underlying snapshotting mechanisms natively inside the controller (these will be delegated to specific runtime drivers).
* Implementing an exhaustive list of Post-Beta features like Snapshot retention, Garbage collection, Networking state, 
configuring auto suspend policies etc.

## Phased Approach

Implementing suspend and resume is complex because snapshotting is runtime-specific. To ensure a safe and manageable delivery, we are rolling out the capabilities in phases.

### Phase 1: Beta
* **Declarative Pausing + Strategy:** Implementing a `spec.suspend: true` boolean field paired with `spec.suspensionStrategy` on the Sandbox CRD. This aligns with standard stateful APIs (like Jobs) and cleanly separates the intent to pause from the physical execution.
* **Pluggable Architecture:** A unified `SnapshotClass` API (mimicking `StorageClass`) to route snapshot commands to the correct runtime provisioner.
* **Manual Triggers:** Suspend and resume actions are manually invoked by developers or external agents modifying the Sandbox specification.

### Phase 2: Post-Beta / Future Additions
* **Automated / Policy-Based Triggers:** Kubernetes controllers automatically suspending sandboxes based on idle timeout policies defined in the sandbox lifecycle configuration.
* **Pre-Suspend / Post-Resume Hooks:** Providing triggers to notify workloads before suspending or after resuming.
* **Snapshot Retention & Garbage Collection:** Configuring the TTL for checkpoints in storage and automatically managing their lifecycle.
* **Status-Driven Resume:** Safely reviving sandboxes by strictly interpreting the `status` block, separating future desire from historical state.
* **Metrics & Observability:** Detailed metrics to track snapshotting operations, including count, latency per provisioner, snapshot size, etc.

## API for Triggering Suspend/Resume in Beta

### Alternatives Considered for Triggering Suspend/Resume

An imperative Kubernetes subresource (like `/suspend` or `/resume`) is an elegant design pattern for executing immediate lifecycle actions. However, standard Custom Resource Definitions (CRDs) are structurally constrained to only support `/status` and `/scale` subresources. To implement triggers for this feature, the following architectural paths were evaluated:

#### 1. API Aggregation Server (True Custom Subresources) - *Rejected*
Instead of using standard CRDs, we could build and deploy a custom API Aggregation Server (using the `apiregistration.k8s.io` API) to support native HTTP endpoints like `POST /apis/agents.x-k8s.io/v1alpha1/namespaces/default/sandboxes/dev-42/suspend`.
* **Pros:** Offers a completely native, secure, and clean API. Enables fine-grained RBAC specifically for the `/suspend` action (e.g., allowing a developer to suspend a sandbox but not delete it).
* **Cons:** Introduces massive operational complexity. Requires maintaining an active API Aggregator Pod, managing custom TLS certificates for API server-to-pod communication, and handling etcd storage manually.

#### 2. Ephemeral "Action" Custom Resources - *Rejected*
Simulating imperative actions by creating a lightweight, temporary CR (e.g., `SandboxAction`) whose sole purpose is to trigger the suspend/resume event.
* **Pros:** 100% standard CRD-compatible and leaves a historical audit trail of who suspended the sandbox and when.
* **Cons:** Creates resource churn in etcd and requires writing a garbage collector to delete these action objects after completion.

#### 3. The `spec.suspend` Boolean + `spec.suspensionStrategy` - *Selected for Beta*
This approach aligns with standard Kubernetes stateful APIs (like the Job API, which uses `spec.suspend` to temporarily pause processing without losing work). The API remains purely declarative by exposing a boolean field (`spec.suspend: true`), with an accompanying `spec.suspensionStrategy` to dictate how the suspension is handled.
* **Pros:**
  * **Semantic Meaning:** Clearly states the sandbox's execution is paused.
  * **Scale Constraints:** Matches the singleton nature of 1-to-1 interactive workspaces perfectly.
  * **SaaS/SDK Readability:** `sandbox.suspend = true` makes instant semantic sense to an LLM developer or application wrapper.
* **Cons:** Lacks native compatibility with Kubernetes `/scale` subresources. (Note: If stateless scale-to-zero compatibility is ever strictly needed, `spec.replicas` could be added later exclusively to trigger a stateless `Stop` strategy, keeping `spec.suspend` as the source of truth for stateful pausing).

#### 4. The `spec.replicas` + `spec.strategy` (The /scale Pivot) - *Rejected*
Overloading the `spec.replicas` field to control stateful hibernation.
* **Pros:** Retains native Kubernetes `/scale` subresource integration out of the box.
* **Cons:** 
  * **Ecosystem Compatibility Risks:** Native tools (HPA, KEDA) expect `replicas: 0` to result in clean-slate deletions.
  * **Accidental Latency & Cloud Costs:** If an autoscaler triggers a scale-down, it expects instant deletion. Initiating a heavy memory checkpoint (e.g., writing 4GB of RAM to an S3 bucket) will block the downscale and potentially cause timeout failures in the autoscaling controller.
  * **Data-Loss Risk:** Users routinely run `replicas=0` as a quick way to clean up resources. Silently saving memory dumps for "deleted" pods can quickly fill cloud storage buckets and blow the storage budget.

## Suspension Strategies Explained

When a Sandbox is marked as `suspend: true`, the physical execution of that pause is dictated by the `spec.suspensionStrategy.type`. We propose three distinct strategies to accommodate different workloads and latency requirements:

| Strategy Type | What `suspend: true` Does | When to Use It |
| :--- | :--- | :--- |
| **Stop** | Standard stateless scale-down. Deletes the Pod and its associated resources. The file system is deleted (unless tied to a Retained PVC). | Stateless agents, clean-slate workspaces, or standard developer playground resets. |
| **Freeze** | Keeps the Pod alive in Kubernetes, but freezes container CPU namespaces. Keeps RAM intact. | Fast-loop interactive agents. Highly responsive (microseconds latency) but actively consumes cluster node memory. |
| **Hibernate** | Serializes the RAM and filesystem to the specified storage class, then terminates the Pod. | Long-idle agents. Drops compute footprint entirely. The "Always-On" illusion. |

## API for Snapshotting: SnapshotClass vs SnapshotProvider

In a Kubernetes-native design, we need a configuration layer for the pluggable provider pattern to tell the agent-sandbox controller how and where to handle snapshotting. We evaluated two patterns: `SnapshotProvider` and `SnapshotClass`.

### SnapshotProvider vs SnapshotClass

* **SnapshotProvider (Strongly Typed):** A strongly typed CRD where fields like S3 bucket names and regions are explicitly defined in the OpenAPI schema.
  * **Pros:** Provides strict validation at the API server level. Catches configuration errors (like typos in field names) immediately upon application.
  * **Cons:** Rigid and tightly coupled. Requires updating the CRD for every new cloud provider or parameter, making it harder to maintain as the ecosystem grows.
  
  *Example (SnapshotProvider):*
  ```yaml
  apiVersion: agents.x-k8s.io/v1beta1
  kind: SnapshotProvider
  metadata:
    name: s3-fast
  spec:
    providerType: "gvisor-s3"
    s3Config: # Strongly typed configuration fields
      bucket: "sandbox-checkpoints"
      region: "us-west-2"
  ```

* **SnapshotClass (StorageClass Paradigm):** Mimics the ubiquitous `PersistentVolumeClaim` -> `StorageClass` workflow. It uses a provisioner string and a free-form `parameters` map.
  * **Pros:** Highly extensible, familiar to Kubernetes admins, and completely decouples the user's workload from the underlying infrastructure implementation.
  * **Cons:** Weaker compile-time validation. Because `parameters` is a free-form map, typos in configuration keys won't be caught by the API server and will fail at runtime.

  *Example (SnapshotClass):*
  ```yaml
  apiVersion: agents.x-k8s.io/v1beta1
  kind: SnapshotClass
  metadata:
    name: gcs-fast
  provisioner: "agents.x-k8s.io/gvisor" # Type of Snapshot Driver
  parameters: # Free-form configuration map
    bucket: "sandbox-checkpoints"
    region: "us-west-2"
  ```

**Preferred: `SnapshotClass` is Selected for Beta.**
While `SnapshotClass` relies on string-based references, it offers significant advantages:
* **Loose Coupling:** Clean separation of concerns. Developers request a class (e.g., `fast-fast-memory-snapshot`), and the platform handles the backend.
* **Familiarity:** It mimics the `StorageClass` paradigm that every Kubernetes administrator already understands.
* **Portability:** The exact same Sandbox spec can run on GKE (mapping to a GCS bucket) and Minikube (mapping to a local directory) without changing a single line in the developer's YAML.

### Architecture

```text
┌─────────────────────────────────────────────────────────────────────────┐
│                    KUBERNETES CONTROL PLANE                             │
│                                                                         │
│   1. USER                       2. OSS AGENT-SANDBOX CONTROLLER         │
│  ┌────────────────────┐        ┌─────────────────────────────────────┐  │
│  │ Sets spec.suspend.   ─────> │ Watches Sandbox state.              │  │
│  │ to true            │        │ Reads the referenced                │  │
│  └────────────────────┘        │ SnapshotClass CRD.                  │  │
│                                └──────────────────┬──────────────────┘  │
│                                                   │                     │
└───────────────────────────────────────────────────┼─────────────────────┘
                                                    │ Passes details to:
                                                    ▼
                       ┌──────────────────────────────────────────┐
                       │ 3. PLUGGABLE SNAPSHOT PROVISIONER
                       ├──────────────────────────────────────────┤
                       │ Orchestrates the low-level checkpoint:   │
                       │ - Executes runtime checkpoint (gVisor)   │
                       │ - Or calls GKE PodSnapshot Controller    │
                       └──────────────────────────────────────────┘
```

### Who uses this CRD?

1. **The Cluster Administrator:** Creates and maintains the `SnapshotClass` resources globally across the cluster. They configure the backend storage parameters (such as cloud buckets and regions) and ensure the necessary container runtimes (e.g., gVisor) are deployed on the nodes to support snapshotting.
2. **The Agent-Sandbox Operator/Controller:** The automated control loop that watches for `Sandbox` state changes. When a sandbox is suspended using the `Hibernate` strategy, the controller reads the referenced `SnapshotClass` and delegates the snapshot execution to the specified `provisioner`.

### API Design

**1. Cluster Admin defines a cluster-wide SnapshotClass:**

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: SnapshotClass
metadata:
  name: fast-memory-snapshot
# StorageClass paradigm: Parameters are a free-form key/value map handled by the provisioner
provisioner: "agents.x-k8s.io/gvisor"
parameters:
  bucket: "s3-sandbox-bucket"
  region: "us-west-2"
```

**2. Developer/Agent references the snapshot class by name:**

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: billing-agent-42
spec:
  # The simple, developer-friendly toggle
  suspend: true
  # The operational instruction manual
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClass: "fast-memory-snapshot" # Name-based snapshot class reference
```

### Suspension Options


```yaml
# 1. THE HOT STRATEGY: Zero latency, high compute cost
spec:
  suspend: true
  strategy:
    type: "Freeze"

---

# 2. THE WARM STRATEGY: Medium latency (RESTORES RAM), zero compute cost, high storage cost
spec:
  suspend: true
  strategy:
    type: "Hibernate"
    hibernate:
      snapshotClass: "fast-memory-snapshot" # Name-based snapshot class reference


---

# 3. THE COLD STRATEGY: High latency (REBOOTS DISK), zero compute cost, low storage cost
spec:
  suspend: true
  strategy:
    type: "Stop" # (Wipes the pod, but keeps the disk)
```


### How will the conditions be exposed?

My existing [PR](https://github.com/kubernetes-sigs/agent-sandbox/pull/422/changes) introduces `Suspended` as a first class
condition to determine the state of the Sandbox. We can extend this status to reflect the snapshot status in the `Reason` field post-beta.

## How Resume Works

The resume operation follows the native, level-triggered controller pattern of Kubernetes by continuously reconciling the desired state specified in `spec` against the observed state of physical cluster resources.

When `spec.suspend` is toggled to `false` (or omitted), the controller executes the following logic sequence during its reconciliation loop:

- **Observe Current State:** The controller queries the API server to check for the existence of the underlying runner Pod associated with the AgentSandbox.
- **Reconcile Discrepancy:** If no active runner Pod is found, the controller interprets this as a mandate to resume/boot the workspace. It invokes the Pod driver to construct a fresh runner Pod shell and securely mounts the existing persistent data volumes containing the workspace state.
- **Reflect Status:** Only after the physical resources have been successfully evaluated or created does the controller update the status block to inform external clients of the current observed state (e.g., setting `status.state` to Running or updating `status.conditions`).
