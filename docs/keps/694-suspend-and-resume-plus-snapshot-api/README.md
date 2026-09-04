# KEP-0694: Suspend and Resume + Snapshot Provider API for Agent Sandboxes

<!-- toc -->
- [Motivation](#motivation)
- [Use Cases](#use-cases)
- [User Personas](#user-personas)
- [Goals](#goals)
- [Non-Goals](#non-goals)
- [Phased Approach](#phased-approach)
  - [Phase 1: Initial Implementation](#phase-1-initial-implementation)
  - [Phase 2: Future Additions](#phase-2-future-additions)
- [API for Triggering Suspend/Resume](#api-for-triggering-suspendresume)
  - [The <code>spec.operatingMode</code> Enum - <em>Implemented</em>](#the-specoperatingmode-enum---implemented)
  - [Alternatives Considered for Triggering Suspend/Resume](#alternatives-considered-for-triggering-suspendresume)
  - [Need for a Snapshotting API](#need-for-a-snapshotting-api)
- [Suspension Strategies Explained](#suspension-strategies-explained)
- [API for Snapshotting: SnapshotClass vs SnapshotProvider](#api-for-snapshotting-snapshotclass-vs-snapshotprovider)
  - [Option A: SnapshotProvider (Strongly Typed CRD)](#option-a-snapshotprovider-strongly-typed-crd)
    - [1. Go API Specification](#1-go-api-specification)
    - [2. Pros and Cons](#2-pros-and-cons)
    - [3. User Interaction](#3-user-interaction)
    - [4. Architecture Flow](#4-architecture-flow)
  - [Option B: SnapshotClass + SnapshotClaim (StorageClass / PVC Paradigm)](#option-b-snapshotclass--snapshotclaim-storageclass--pvc-paradigm)
    - [1. Go API Specification](#1-go-api-specification-1)
    - [2. Pros and Cons](#2-pros-and-cons-1)
    - [3. User Interaction](#3-user-interaction-1)
    - [4. Architecture Flow](#4-architecture-flow-1)
  - [Who uses these CRDs?](#who-uses-these-crds)
  - [Suspension Options](#suspension-options)
  - [How will the conditions be exposed?](#how-will-the-conditions-be-exposed)
- [Snapshot Driver gRPC Interface](#snapshot-driver-grpc-interface)
  - [Protobuf Service Definition](#protobuf-service-definition)
  - [Where the Snapshot Result Lives (Sandbox Status)](#where-the-snapshot-result-lives-sandbox-status)
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

## Goals

* Provide trigger for manual suspension and resumption of sandboxes.
* Provide a unified, pluggable API that abstracts runtime-specific snapshotting implementations (e.g., gVisor, Firecracker) and separates infrastructure configuration (Cluster Admin) from sandbox lifecycle requests (Developer/Agent).

## Non-Goals

* Implementing the underlying snapshotting mechanisms natively inside the controller (these will be delegated to specific runtime drivers).
* Implementing an exhaustive list of extendible features like Snapshot retention, Garbage collection, Networking state, configuring auto suspend policies etc.

## Phased Approach

Implementing suspend and resume is complex because snapshotting is runtime-specific. To ensure a safe and manageable delivery, we are rolling out the capabilities in phases.

### Phase 1: Initial Implementation
* **Declarative Pausing via OperatingMode (Implemented):** Implemented a `spec.operatingMode: Running | Suspended` enum field on the Sandbox CRD to dictate the execution state. This aligns with standard Kubernetes API conventions by avoiding rigid booleans and preventing mutually exclusive field conflicts. Suspend and resume actions are manually invoked by developers or external agents modifying the Sandbox specification.
* **Suspension Strategy Configuration (Pending):** Introduce the `spec.suspensionStrategy` field to allow configuring different suspension styles (Stop, Freeze, Hibernate) when `operatingMode` is set to `Suspended`.
* **Pluggable Architecture (Pending):** A unified Snapshot API to route snapshot commands to the correct runtime driver.

### Phase 2: Future Additions
* **Automated / Policy-Based Triggers:** Kubernetes controllers automatically suspending sandboxes based on idle timeout policies defined in the sandbox lifecycle configuration (see [PR #972](https://github.com/kubernetes-sigs/agent-sandbox/pull/972/changes)).
* **Pre-Suspend / Post-Resume Hooks:** Providing triggers to notify workloads before suspending or after resuming.
* **Snapshot Retention & Garbage Collection:** Configuring the TTL for checkpoints in storage and automatically managing their lifecycle.
* **Metrics & Observability:** Detailed metrics to track snapshotting operations, including count, latency per driver, snapshot size, etc.

## API for Triggering Suspend/Resume

### The `spec.operatingMode` Enum - *Implemented*

This approach introduced a strongly-typed Enum (`spec.operatingMode: Running | Suspended`) block (designed and implemented in [KEP-0694: Suspend and Resume for Agent Sandboxes Beta](../694-kep-for-suspend-and-resume-for-beta/README.md)) to dictate how the suspension is physically handled.

* **Pros:**
  * **API Compliant:** Aligns perfectly with Kubernetes API conventions by avoiding rigid booleans and preventing mutually exclusive field conflicts.
  * **Semantic Meaning:** Clearly states the sandbox's execution state while leaving room for future expansion.
  * **Scale Constraints:** Matches the singleton nature of 1-to-1 interactive workspaces perfectly.
* **Cons:**
  * **Loss of Native Scaling:** By replacing `spec.replicas` with an enum, we lose out-of-the-box integration with native Kubernetes scaling tools (like HPA or KEDA) that depend on the `/scale` subresource. Although, we currently don't have plans around scaling Pods for a single Sandbox.

### Alternatives Considered for Triggering Suspend/Resume

The detailed analysis of alternative trigger designs (such as API aggregation, action resources, booleans, and scheduling gates) is documented in the [694-kep-for-suspend-and-resume-for-beta/README.md](../694-kep-for-suspend-and-resume-for-beta/README.md).

### Need for a Snapshotting API

While `spec.operatingMode` controls the target execution state, the mechanism used to pause the sandbox depends on the desired suspension strategy. As discussed below, strategies like **Stop** (stateless termination) and **Freeze** (process freezing in memory) do not require state serialization. However, the **Hibernate** strategy (which completely tears down compute resources while persisting process memory and filesystem state) depends on a pluggable snapshotting mechanism. The `SnapshotClass` and `SnapshotProvider` APIs defined in this proposal are specifically designed to coordinate this serialization and deserialization process for stateful suspension.

## Suspension Strategies Explained

When a Sandbox is marked as `operatingMode: Suspended`, the physical execution of that pause is dictated by the `spec.suspensionStrategy.type`. We propose three distinct strategies to accommodate different workloads and latency requirements:

| Strategy Type | What `operatingMode: Suspended` Does | When to Use It |
| :--- | :--- | :--- |
| **Stop** | Standard stateless scale-down. Deletes the Pod and its associated resources. The file system is deleted (unless tied to a Retained PVC). | Stateless agents, clean-slate workspaces, or standard developer playground resets. |
| **Freeze** | Keeps the Pod alive in Kubernetes, but freezes container CPU namespaces. Keeps RAM intact. | Fast-loop interactive agents. Highly responsive (microseconds latency) but actively consumes cluster node memory. |
| **Hibernate** | Serializes the RAM and filesystem to the specified storage class, then terminates the Pod. | Long-idle agents. Drops compute footprint entirely. The "Always-On" illusion. |

## API for Snapshotting: SnapshotClass vs SnapshotProvider

In a Kubernetes-native design, we need a configuration layer for the pluggable provider pattern to tell the agent-sandbox controller how and where to handle snapshotting. We are still actively discussing these two options.

### Option A: SnapshotProvider (Strongly Typed CRD)

This option introduces a custom cluster-scoped resource `SnapshotProvider` with a strongly-typed schema validating provider-specific configurations.

#### 1. Go API Specification

```go
// SnapshotProviderSpec defines the configurations for the snapshot provider.
type SnapshotProviderSpec struct {
	// providerType specifies the type of pluggable snapshot driver (e.g., "gvisor", "firecracker").
	// +required
	ProviderType string `json:"providerType"`

	// s3 contains configuration options specific to AWS S3 / S3-compatible endpoints.
	// +optional
	S3 *S3SnapshotConfig `json:"s3,omitempty"`

	// gcs contains configuration options specific to Google Cloud Storage.
	// +optional
	GCS *GCSSnapshotConfig `json:"gcs,omitempty"`
}

type S3SnapshotConfig struct {
	// bucket is the name of the S3 bucket to save checkpoints.
	// +required
	Bucket string `json:"bucket"`

	// region is the AWS region of the bucket.
	// +optional
	Region string `json:"region,omitempty"`
}

type GCSSnapshotConfig struct {
	// bucket is the name of the GCS bucket to save checkpoints.
	// +required
	Bucket string `json:"bucket"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type SnapshotProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SnapshotProviderSpec `json:"spec"`
}

// Sandbox modification to reference SnapshotProvider:
type HibernateStrategy struct {
	// snapshotProviderRef references the SnapshotProvider configuration.
	// +required
	SnapshotProviderRef LocalObjectReference `json:"snapshotProviderRef"`
}
```

#### 2. Pros and Cons

* **Pros:**
  * **Strict Schema Validation:** Configuration options (such as AWS bucket name and region) are explicitly validated by the Kubernetes API server at request-creation time. Misconfigured or missing required fields are rejected immediately.
  * **Self-Documenting:** The API specification clearly reveals all supported cloud platforms and parameters.
* **Cons:**
  * **Tight Coupling:** Adding support for a new cloud provider or new driver options requires modifying the core CRD schema and releasing a new version of the API.
  * **Poor Portability:** The `Sandbox` spec directly targets a provider instance config, meaning copying the Sandbox YAML between environments (e.g., dev/prod, GCP/AWS) requires editing the provider details.

#### 3. User Interaction

**Cluster Admin defines the SnapshotProvider:**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: SnapshotProvider
metadata:
  name: s3-fast
spec:
  providerType: "gvisor-s3"
  s3:
    bucket: "sandbox-checkpoints"
    region: "us-west-2"
```

**Developer/Agent references the provider in Sandbox:**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: billing-agent-42
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotProviderRef:
        name: "s3-fast"
```

#### 4. Architecture Flow

```mermaid
sequenceDiagram
    actor User as Developer / Agent
    participant KubeAPIServer as Kube API Server
    participant Controller as Sandbox Controller
    participant SnapshotProvider as SnapshotProvider CR
    participant Driver as Snapshot Driver (e.g. gVisor-S3)
    participant Pod as Sandbox Pod

    User->>KubeAPIServer: Update Sandbox (operatingMode = Suspended, providerRef = s3-fast)
    Controller->>KubeAPIServer: Watch Event: Sandbox updated
    Controller->>KubeAPIServer: Fetch SnapshotProvider 's3-fast'
    KubeAPIServer-->>Controller: Return SnapshotProvider CR (with strongly-typed spec.s3Config)
    Controller->>Driver: Route checkpoint request with S3 configurations
    Driver->>Pod: Checkpoint memory & filesystem state
    Pod-->>Driver: State serialized & uploaded to S3
    Driver-->>Controller: Checkpoint completed successfully
    Controller->>KubeAPIServer: Terminate Sandbox Pod
    Controller->>KubeAPIServer: Update Sandbox Status (Condition Suspended = True)
```

***

### Option B: SnapshotClass + SnapshotClaim (StorageClass / PVC Paradigm)

This option follows the standard Kubernetes `StorageClass` / `PersistentVolumeClaim` split and extends it with a template layer that mirrors Dynamic Resource Allocation (DRA)'s `DeviceClass` / `ResourceClaim` / `ResourceClaimTemplate` pattern. It introduces three resources so that admin-owned backend configuration, namespace-owned storage targets, and per-Sandbox claim generation are cleanly separated:

* **`SnapshotClass`** (cluster-scoped, admin-owned): answers *"what kind of snapshot backend is this, and which driver runs it?"* It names the pluggable `provisioner` (e.g. gVisor, Firecracker) and carries admin-owned backend defaults.
* **`SnapshotClaim`** (namespaced, tenant-owned): the PVC-like request that selects a `SnapshotClass` and carries the namespace-local storage target (bucket / container / region / prefix). It is a **shared** resource — every `Sandbox` that references the same claim by name uses that single claim and its one storage target, just as multiple Pods can mount the same PVC. The `Sandbox` references a claim by name, never a class directly.
* **`SnapshotClaimTemplate`** (namespaced, tenant-owned): a stamp for **generating a distinct `SnapshotClaim` per Sandbox**, analogous to DRA's `ResourceClaimTemplate`. Instead of sharing one claim, each `Sandbox` that references the template gets its **own** generated `SnapshotClaim` — even when the template carries no Sandbox-specific parameters — owned by and lifetime-bound to that Sandbox. The template holds the complete claim spec; the controller copies it verbatim and derives any per-Sandbox uniqueness (e.g. the storage key/prefix) from the Sandbox's identity, exactly as DRA generates a separate `ResourceClaim` per Pod.

Inserting the namespaced claim layer between `Sandbox` and `SnapshotClass` (exactly as a PVC sits between a Pod and a `StorageClass`) makes storage ownership unambiguous in multi-tenant clusters: cluster admins own the pluggable driver catalog, while namespace operators own where their checkpoints land.

#### 1. Go API Specification

```go
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ssclass
type SnapshotClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// provisioner indicates the pluggable driver responsible for execution
	// (e.g. "agents.x-k8s.io/gvisor").
	// +required
	Provisioner string `json:"provisioner"`

	// parameters holds admin-owned, backend-specific defaults handled by the
	// provisioner (e.g. endpoints, local base paths, compatibility flags).
	// Namespace-local target fields (bucket/container/region/prefix) belong on
	// SnapshotClaim, not here.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// SnapshotClaimSpec is namespace-local configuration shared by one or more Sandboxes.
type SnapshotClaimSpec struct {
	// snapshotClassName selects the cluster-scoped SnapshotClass. When empty, the
	// controller uses the annotated cluster default SnapshotClass.
	// +optional
	SnapshotClassName string `json:"snapshotClassName,omitempty"`

	// parameters holds namespace-owned, backend-specific target information
	// (e.g. region, bucket/container, prefix, storage account, repository).
	// Optional backend tunables such as compression are expressed here as
	// parameter keys (e.g. "compression": "zstd") rather than as dedicated fields.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// SnapshotClaimStatus records class resolution only. The snapshot execution
// result lives on the Sandbox status, not on the claim.
type SnapshotClaimStatus struct {
	// conditions represent the latest available observations of the claim's
	// resolution state, following the standard Kubernetes condition convention.
	// The well-known condition type is "Bound" (see SnapshotClaimConditionBound),
	// whose status + reason subsume the former Pending/Bound/Failed phase values.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// snapshotClassName is the resolved SnapshotClass, including defaulting.
	// +optional
	SnapshotClassName string `json:"snapshotClassName,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Well-known SnapshotClaim condition types. Following Kubernetes convention we
// use a single condition type whose status/reason express what the old phase
// enum tracked, rather than one boolean condition per state.
const (
	// SnapshotClaimConditionBound reports whether the claim has resolved to a
	// SnapshotClass and storage target.
	SnapshotClaimConditionBound = "Bound"
)

// Reasons for the Bound condition, mapping 1:1 onto the former phase values:
//
//	Bound = Unknown, reason = "Pending" -> claim not yet resolved (was phase "Pending")
//	Bound = True,    reason = "Bound"   -> resolved successfully   (was phase "Bound")
//	Bound = False,   reason = "Failed"  -> resolution failed       (was phase "Failed")
const (
	SnapshotClaimReasonPending = "Pending"
	SnapshotClaimReasonBound   = "Bound"
	SnapshotClaimReasonFailed  = "Failed"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snapclaim
type SnapshotClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SnapshotClaimSpec   `json:"spec,omitempty"`
	Status SnapshotClaimStatus `json:"status,omitempty"`
}

// SnapshotClaimTemplateSpec mirrors DRA ResourceClaimTemplate: metadata is copied
// onto generated claims, and spec is the SnapshotClaimSpec to copy.
type SnapshotClaimTemplateSpec struct {
	// +optional
	Metadata metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SnapshotClaimSpec `json:"spec"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=snapclaimtmpl
type SnapshotClaimTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SnapshotClaimTemplateSpec `json:"spec"`
}

// SuspensionStrategy is the type of spec.suspensionStrategy: it selects how a
// Suspended Sandbox is physically paused and, for Hibernate, where the
// checkpoint is stored.
type SuspensionStrategy struct {
	// type selects the pause mechanism applied when operatingMode is Suspended.
	// +required
	Type SuspensionType `json:"type"`

	// hibernate configures the Hibernate strategy and is required when type is
	// "Hibernate" (Stop and Freeze need no state serialization and ignore it).
	// +optional
	Hibernate *HibernateStrategy `json:"hibernate,omitempty"`
}

// SuspensionType enumerates the supported suspension strategies.
// +kubebuilder:validation:Enum=Stop;Freeze;Hibernate
type SuspensionType string

const (
	// SuspensionTypeStop deletes the Pod (stateless scale-down).
	SuspensionTypeStop SuspensionType = "Stop"
	// SuspensionTypeFreeze keeps the Pod alive but freezes its containers.
	SuspensionTypeFreeze SuspensionType = "Freeze"
	// SuspensionTypeHibernate serializes RAM + filesystem, then deletes the Pod.
	SuspensionTypeHibernate SuspensionType = "Hibernate"
)

// HibernateStrategy is the type of spec.suspensionStrategy.hibernate. It selects
// the snapshot storage by referencing EITHER an existing shared SnapshotClaim OR
// a SnapshotClaimTemplate that the controller stamps into a per-Sandbox
// SnapshotClaim. The two references are mutually exclusive, mirroring a Pod's
// resourceClaimName vs resourceClaimTemplateName choice in DRA.
type HibernateStrategy struct {
	// snapshotClaimName references an existing, shared SnapshotClaim in the
	// Sandbox namespace. Every Sandbox that names the same claim shares it (and
	// its single storage target), like multiple Pods mounting one PVC. When both
	// this and snapshotClaimTemplateName are empty, the controller falls back to
	// the namespace/cluster default claim.
	// +optional
	SnapshotClaimName string `json:"snapshotClaimName,omitempty"`

	// snapshotClaimTemplateName references a SnapshotClaimTemplate in the Sandbox
	// namespace. The controller generates a distinct SnapshotClaim per Sandbox
	// from the template's complete spec (copied verbatim), owns it via the
	// Sandbox, and binds its lifetime to the Sandbox. Each referencing Sandbox
	// gets its own generated claim even when the template carries no
	// Sandbox-specific parameters. Mutually exclusive with snapshotClaimName.
	// +optional
	SnapshotClaimTemplateName string `json:"snapshotClaimTemplateName,omitempty"`
}
```

#### 2. Pros and Cons

* **Pros:**
  * **Loose Coupling:** Clean separation of concerns. Pluggable drivers can be registered without modifying the core `SnapshotClass`, `SnapshotClaim`, or `Sandbox` API specs.
  * **UX Familiarity:** Follows the widely understood `StorageClass` / PVC workflow, plus the DRA `ResourceClaimTemplate` pattern for per-Sandbox generation.
  * **Portability:** High portability across environments. The `Sandbox` spec only requests a claim by name; the platform resolves it to a class and storage target that can differ per cloud/cluster without changing the `Sandbox` YAML.
  * **Clear Multi-Tenant Ownership:** Admins own the cluster-scoped `SnapshotClass` catalog; namespace operators own the `SnapshotClaim` / `SnapshotClaimTemplate` that decide where their checkpoints are written.
* **Cons:**
  * **Weak Schema Validation:** Because `parameters` is a free-form `map[string]string`, typos or invalid configs are not validated at request time and only surface during claim resolution or runtime reconciliation.
  * **More Moving Parts:** Three resources plus a claim-resolution step add indirection compared to a single strongly-typed provider CRD (Option A).

#### 3. User Interaction

**Cluster Admin defines the `SnapshotClass` (and optionally marks a cluster default):**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: SnapshotClass
metadata:
  name: gvisor-fast
  annotations:
    snapshotclass.agents.x-k8s.io/is-default-class: "true"
provisioner: "agents.x-k8s.io/gvisor"
parameters:
  endpoint: "https://s3.us-west-2.amazonaws.com"
```

**Namespace operator defines a `SnapshotClaim` (shared namespace target):**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: SnapshotClaim
metadata:
  name: default-claim
  namespace: team-a
spec:
  snapshotClassName: gvisor-fast   # empty => cluster default
  parameters:
    region: "us-west-2"
    bucket: "sandbox-checkpoints"
    prefix: "team-a/shared"
```

**Namespace operator defines a `SnapshotClaimTemplate` (for per-Sandbox generated claims):**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: SnapshotClaimTemplate
metadata:
  name: team-a-s3
  namespace: team-a
spec:
  metadata:
    labels:
      agents.x-k8s.io/snapshot-profile: team-a-s3
  spec:
    snapshotClassName: gvisor-fast
    parameters:
      region: "us-west-2"
      bucket: "sandbox-checkpoints"
      prefix: "team-a"   # the platform appends a per-Sandbox suffix
```

**Developer/Agent references a *shared* claim in `Sandbox` (one claim, many Sandboxes):**
```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: billing-agent-42
  namespace: team-a
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClaimName: "default-claim"   # shared: every Sandbox naming "default-claim" uses the same claim
```

**Developer/Agent references a *template* in `Sandbox` (one generated claim per Sandbox):**

Referencing `snapshotClaimTemplateName` instead makes the controller stamp out a
**separate** `SnapshotClaim` for each Sandbox from the `team-a-s3` template above —
even though neither Sandbox supplies any extra parameters. Each generated claim is
owned by its Sandbox (deleted with it), and the driver derives a unique storage key
from the Sandbox identity so the two Sandboxes never collide:

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: billing-agent-42
  namespace: team-a
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClaimTemplateName: "team-a-s3"   # generated: this Sandbox gets its OWN claim
---
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: research-agent-7
  namespace: team-a
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClaimTemplateName: "team-a-s3"   # same template -> a DIFFERENT generated claim
```

The controller creates one `SnapshotClaim` per Sandbox — e.g.
`billing-agent-42-team-a-s3-<hash>` and `research-agent-7-team-a-s3-<hash>` — each a
verbatim copy of the template's `spec` and `ownerReferences`-linked to its Sandbox.
This mirrors how DRA generates a distinct `ResourceClaim` per Pod from a
`ResourceClaimTemplate`.

#### 4. Architecture Flow

```mermaid
sequenceDiagram
    actor User as Developer / Agent
    participant KubeAPIServer as Kube API Server
    participant Controller as Sandbox Controller
    participant SnapshotClaim as SnapshotClaim CR
    participant SnapshotClass as SnapshotClass CR
    participant Driver as Pluggable Snapshot Driver
    participant Pod as Sandbox Pod

    User->>KubeAPIServer: Update Sandbox (operatingMode = Suspended, snapshotClaimName = default-claim)
    Controller->>KubeAPIServer: Watch Event: Sandbox updated
    Controller->>KubeAPIServer: Fetch SnapshotClaim 'default-claim' (same namespace)
    KubeAPIServer-->>Controller: Return SnapshotClaim (snapshotClassName='gvisor-fast', parameters={bucket, region, prefix})
    Controller->>KubeAPIServer: Fetch SnapshotClass 'gvisor-fast' (or annotated default)
    KubeAPIServer-->>Controller: Return SnapshotClass (provisioner='gvisor', admin parameters)
    Controller->>Driver: Route checkpoint request (class provisioner + merged claim parameters)
    Driver->>Pod: Checkpoint memory & filesystem state
    Pod-->>Driver: State serialized & uploaded to the claim's storage target
    Driver-->>Controller: Checkpoint completed successfully
    Controller->>KubeAPIServer: Terminate Sandbox Pod
    Controller->>KubeAPIServer: Update Sandbox Status (Condition Suspended = True)
```

> A `SnapshotClaim` referenced by `snapshotClaimName` is **shared**: every Sandbox that names it points at the same claim and the same storage target. A `SnapshotClaimTemplate` referenced by `snapshotClaimTemplateName` is **generative**: the controller stamps out a distinct `SnapshotClaim` per Sandbox by copying the template's `spec` verbatim, sets the Sandbox as its owner (so it is garbage-collected with the Sandbox), and lets the driver derive a per-Sandbox storage key from the Sandbox identity. Either way the checkpoint path only ever reads a resolved `SnapshotClaim`, so the class-resolution flow above is identical — the only difference is whether that claim is shared or generated. This mirrors DRA's `resourceClaimName` (shared) vs `resourceClaimTemplateName` (one generated `ResourceClaim` per Pod) distinction.

***

### Who uses these CRDs?

1. **The Cluster Administrator:** Creates and maintains the cluster-scoped `SnapshotClass` (Option B) or `SnapshotProvider` (Option A) resources globally. They register the pluggable drivers, configure admin-owned backend defaults, and ensure nodes support runtime snapshotting (e.g. gVisor).
2. **The Namespace / Tenant Operator (Option B):** Owns the namespaced `SnapshotClaim` and `SnapshotClaimTemplate` resources. They decide where their namespace's checkpoints land (bucket / container / region / prefix) and can offer templates for per-Sandbox generated claims, without touching the cluster-scoped class catalog.
3. **The Agent-Sandbox Operator/Controller:** Watches Sandbox state changes. When `operatingMode` is `Suspended` and the strategy is `Hibernate`, it resolves the referenced claim to a class, then delegates the action to the corresponding driver.

### Suspension Options

```yaml
# 1. THE HOT STRATEGY: Zero latency, high compute cost
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Freeze"

---

# 2. THE WARM STRATEGY: Medium latency (RESTORES RAM), zero compute cost, high storage cost
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Hibernate"
    hibernate:
      snapshotClaimName: "fast-memory-snapshot" # Name-based snapshot claim reference (resolves to a SnapshotClass)

---

# 3. THE COLD STRATEGY: High latency (REBOOTS DISK), zero compute cost, low storage cost
spec:
  operatingMode: "Suspended"
  suspensionStrategy:
    type: "Stop" # (Wipes the pod, but keeps the disk)
```


### How will the conditions be exposed?

The status of the suspension/resumption process is exposed through a first-class `Suspended` condition on the `Sandbox` resource. The detailed condition transition matrix, state diagram, and controller implementation are documented in the [119-sandbox-suspended-state/README.md](../119-sandbox-suspended-state/README.md).

## Snapshot Driver gRPC Interface

To maintain a pluggable architecture and decouple the core `agent-sandbox` controller from specific hypervisor or container runtime snapshotting APIs (like `gVisor` checkpointing or `Firecracker` microVM state serialization), drivers MUST implement a standard gRPC service. The controller communicates with the driver over a Unix Domain Socket (UDS) or a secure TCP endpoint specified in the cluster configuration.

### Protobuf Service Definition

```protobuf
syntax = "proto3";

package agents.x-k8s.io.v1beta1;

option go_package = "sigs.k8s.io/agent-sandbox/clients/go/pkg/apis/snapshot/v1";

// SnapshotDriver defines the RPC interface that pluggable snapshot drivers
// must implement to support Hibernate and Freeze strategies.
service SnapshotDriver {
  // Checkpoint serializes the memory and filesystem state of a running sandbox pod.
  rpc Checkpoint(CheckpointRequest) returns (CheckpointResponse);

  // Restore prepares and binds the saved state prior to the Pod shell boot.
  rpc Restore(RestoreRequest) returns (RestoreResponse);

  // Delete purges a saved checkpoint from the backend storage.
  rpc Delete(DeleteRequest) returns (DeleteResponse);
}

message CheckpointRequest {
  // Unique name identifying the sandbox resource.
  string sandbox_name = 1;
  string sandbox_namespace = 2;

  // The target running Pod name in the cluster.
  string pod_name = 3;

  // Key-value configuration parameters passed directly from the SnapshotClass.
  map<string, string> parameters = 4;
}

message CheckpointResponse {
  // Unique ID of the created snapshot, used for subsequent restore/delete.
  string snapshot_uid = 1;

  // Size of the serialized state in bytes.
  int64 size_bytes = 2;

  // Opaque metadata returned by the driver to store on the Sandbox status.
  map<string, string> driver_metadata = 3;
}

message RestoreRequest {
  string sandbox_name = 1;
  string sandbox_namespace = 2;

  // The unique ID of the snapshot to restore from.
  string snapshot_uid = 3;

  // Key-value configurations passed from the SnapshotClass.
  map<string, string> parameters = 4;
}

message RestoreResponse {
  // Opaque instructions returned by the driver (e.g. volume mounts, env vars)
  // that the controller should inject into the restored Pod shell spec.
  map<string, string> restore_metadata = 1;
}

message DeleteRequest {
  // The unique ID of the snapshot to delete.
  string snapshot_uid = 1;

  // Key-value configurations passed from the SnapshotClass.
  map<string, string> parameters = 2;
}

message DeleteResponse {}
```

### Where the Snapshot Result Lives (Sandbox Status)

A `SnapshotClaim` is deliberately **not** the snapshot artifact. Unlike a `PersistentVolumeClaim`, which binds 1:1 to a `PersistentVolume` that holds the concrete volume metadata, a `SnapshotClaim` is a *reusable storage target* (class + bucket/prefix/region) that can back many checkpoints over time and be shared across Sandboxes. There is therefore no single "PV" to bind to, and `SnapshotClaimStatus` intentionally carries only class-resolution state.

The concrete artifact metadata produced by `CheckpointResponse` (the driver-assigned handle, size, and opaque driver blob) instead lives on the **`Sandbox` status**. Each `Sandbox` has at most one live checkpoint to resume from, so the result is owned by — and lifetime-bound to — the Sandbox that created it, rather than a shared claim or a separate cluster-scoped object. This is the piece a `PersistentVolume` holds in the PVC world and that `VolumeSnapshotContent` holds in CSI.

```go
// SandboxStatus additions (existing conditions/observedGeneration fields omitted).
type SandboxStatus struct {
	// snapshot records the most recent successful checkpoint for this Sandbox.
	// It is populated from the driver's CheckpointResponse when a Suspend
	// completes and is consumed by the resume path (Restore) to rehydrate the
	// Pod. The controller clears it once the checkpoint is deleted or
	// invalidated.
	// +optional
	Snapshot *SandboxSnapshotStatus `json:"snapshot,omitempty"`
}

// SandboxSnapshotStatus is the concrete snapshot artifact metadata — the record
// a PV holds in the PVC world and that VolumeSnapshotContent holds in CSI.
type SandboxSnapshotStatus struct {
	// snapshotUID is the driver-assigned handle used for Restore and Delete
	// (CheckpointResponse.snapshot_uid).
	// +required
	SnapshotUID string `json:"snapshotUID"`

	// snapshotClassName is the resolved SnapshotClass the checkpoint was written
	// with, recorded so Restore/Delete target the same backend even if the
	// claim's class resolution later changes.
	// +required
	SnapshotClassName string `json:"snapshotClassName"`

	// snapshotClaimName is the SnapshotClaim (shared or generated) that supplied
	// the storage target for this checkpoint.
	// +optional
	SnapshotClaimName string `json:"snapshotClaimName,omitempty"`

	// sizeBytes is the serialized state size reported by the driver
	// (CheckpointResponse.size_bytes).
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// creationTime is when the checkpoint completed.
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// driverMetadata is the opaque key/value blob the driver returned
	// (CheckpointResponse.driver_metadata), passed back verbatim on Restore.
	// +optional
	DriverMetadata map[string]string `json:"driverMetadata,omitempty"`
}
```

> **Side note — Alternative (Option 2): a dedicated `Snapshot` CR.** Instead of recording the artifact inline on `Sandbox.status` (Option 1 above), we could introduce a separate `Snapshot` resource — the true analogue of a `PersistentVolume` in the PVC world, or of `VolumeSnapshotContent` in CSI. The `Sandbox` (or `SnapshotClaim`) would *bind* to a `Snapshot` object that holds the immutable artifact metadata (`snapshotUID`, `sizeBytes`, `sourceSandbox`, `snapshotClassName`, `driverMetadata`, `creationTime`).
>
> * **Pros:**
>   * **First-class artifacts:** Snapshots become independently listable, labelable, and `kubectl get`-able, instead of being buried in a Sandbox's status.
>   * **Decoupled lifetime:** A `Snapshot` can outlive its source `Sandbox`, enabling retention, audit, and garbage-collection policies (Phase 2 goals) without keeping the Sandbox around.
>   * **Cross-Sandbox restore / fork:** A different Sandbox can restore from an existing `Snapshot`, directly supporting the "starter clone" and "rewind & fork" use cases.
>   * **Multiple snapshots per source:** Naturally models a history of checkpoints, not just the single most-recent one.
> * **Cons:**
>   * **More moving parts:** Adds a fourth resource plus binding, ownership, and finalizer/GC semantics that must be specified and reconciled.
>   * **Lifecycle complexity:** Requires rules for orphaned snapshots, retention/TTL, and reclaim behavior (analogous to PV `reclaimPolicy`) — most of which are explicit Non-Goals for Phase 1.
>   * **Heavier for the common case:** For a singleton Sandbox that only ever resumes from its own latest checkpoint, a whole CR is more machinery than the inline status field needs.
>
> Option 1 (inline `Sandbox.status`) is proposed for Phase 1 because it matches the current gRPC contract and the singleton resume model; a dedicated `Snapshot` CR is a natural Phase 2 evolution if retention, sharing, or fork/rewind workflows are prioritized.

## How Resume Works

The resume operation follows the native, level-triggered controller pattern of Kubernetes by continuously reconciling the desired state specified in `spec` against the observed state of physical cluster resources.

> **Note:** The "does a snapshot exist for this Sandbox?" lookup below depends directly on where the snapshot metadata is stored (see [Where the Snapshot Result Lives](#where-the-snapshot-result-lives-sandbox-status)). Under **Option 1** (inline `Sandbox.status.snapshot`), the controller simply reads the artifact metadata off the Sandbox it is already reconciling — no extra lookup. Under **Option 2** (a dedicated `Snapshot` CR), the controller instead resolves the bound `Snapshot` object (e.g. via a `spec` reference or an owner/label selector) to obtain the `snapshotUID` and driver metadata before calling `Restore`. The rest of the sequence is identical; only the source of the checkpoint metadata differs.

When `spec.operatingMode` is set to `Running` (or omitted), the controller executes the following logic sequence during its reconciliation loop:

- **Observe Current State:** The controller queries the API server to check for the existence of the underlying runner Pod associated with the AgentSandbox.
- **Evaluate Restore Necessity:** If no active runner Pod is found, the controller checks if a snapshot of the memory/filesystem state exists for this Sandbox.
- **Reconcile Discrepancy (Restore or Boot):**
  - **Stateful Restore (Hibernate/Freeze):** If a snapshot exists, the controller reads the `snapshotClassName` configuration referenced in `spec.suspensionStrategy` to retrieve storage backend parameters. It coordinates with the pluggable snapshot driver to restore the live process memory and filesystem state into a newly provisioned runner Pod shell.
  - **Cold Boot (Stop / Default):** If no snapshot exists, the controller invokes the Pod driver to construct a fresh runner Pod shell from scratch using the Sandbox's original `spec.podTemplate` and binds it to the existing Persistent Volume Claims (PVCs).
- **Reflect Status:** Only after physical resources are successfully restored or created and the Pod transitions to the running state does the controller update the status block to inform external clients of the current observed state (e.g., setting the `Suspended` condition to `False` and `Ready` condition to `True`).