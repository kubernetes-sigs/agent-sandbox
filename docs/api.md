# API Reference

## Packages
- [agents.x-k8s.io/v1alpha1](#agentsx-k8siov1alpha1)
- [extensions.agents.x-k8s.io/v1alpha1](#extensionsagentsx-k8siov1alpha1)


## agents.x-k8s.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types
- [Sandbox](#sandbox)





#### EmbeddedObjectMetadata







_Appears in:_
- [PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name must be unique within a namespace. Is required when creating resources, although<br />some resources may allow a client to request the generation of an appropriate name<br />automatically. Name is primarily intended for creation idempotence and configuration<br />definition.<br />Cannot be updated.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names |  |  |
| `labels` _object (keys:string, values:string)_ | Map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  |  |
| `annotations` _object (keys:string, values:string)_ | Annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  |  |


#### Lifecycle



Lifecycle defines the lifecycle management for the Sandbox.



_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | ShutdownTime is the absolute time when the sandbox expires. |  | Format: date-time <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | ShutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.<br />Underlying resources(Pods, Services) are always deleted on expiry. | Retain | Enum: [Delete Retain] <br /> |


#### PersistentVolumeClaimTemplate







_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[EmbeddedObjectMetadata](#embeddedobjectmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[PersistentVolumeClaimSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#persistentvolumeclaimspec-v1-core)_ | Spec is the PVC's spec |  | Required: \{\} <br /> |


#### PodMetadata







_Appears in:_
- [PodTemplate](#podtemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `labels` _object (keys:string, values:string)_ | Map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  |  |
| `annotations` _object (keys:string, values:string)_ | Annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  |  |


#### PodTemplate







_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `spec` _[PodSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#podspec-v1-core)_ | Spec is the Pod's spec |  | Required: \{\} <br /> |
| `metadata` _[PodMetadata](#podmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |


#### Sandbox



Sandbox is the Schema for the sandboxes API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `Sandbox` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SandboxSpec](#sandboxspec)_ | spec defines the desired state of Sandbox |  |  |
| `status` _[SandboxStatus](#sandboxstatus)_ | status defines the observed state of Sandbox |  |  |


#### SandboxSpec



SandboxSpec defines the desired state of Sandbox



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | PodTemplate describes the pod spec that will be used to create an agent sandbox. |  | Required: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | VolumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.<br />Every claim in this list must have at least one matching access mode with a provisioner volume. |  | Optional: \{\} <br /> |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | ShutdownTime is the absolute time when the sandbox expires. |  | Format: date-time <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | ShutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.<br />Underlying resources(Pods, Services) are always deleted on expiry. | Retain | Enum: [Delete Retain] <br /> |
| `replicas` _integer_ | Replicas is the number of desired replicas.<br />The only allowed values are 0 and 1.<br />Defaults to 1. |  | Maximum: 1 <br />Minimum: 0 <br /> |


#### SandboxStatus



SandboxStatus defines the observed state of Sandbox.



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serviceFQDN` _string_ | FQDN that is valid for default cluster settings<br />Limitation: Hardcoded to the domain .cluster.local<br />e.g. sandbox-example.default.svc.cluster.local |  |  |
| `service` _string_ | e.g. sandbox-example |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | status conditions array |  |  |
| `replicas` _integer_ | Replicas is the number of actual replicas. |  |  |
| `selector` _string_ | LabelSelector is the label selector for pods. |  |  |


#### ShutdownPolicy

_Underlying type:_ _string_

ShutdownPolicy describes the policy for deleting the Sandbox when it expires.

_Validation:_
- Enum: [Delete Retain]

_Appears in:_
- [Lifecycle](#lifecycle)
- [SandboxSpec](#sandboxspec)

| Field | Description |
| --- | --- |
| `Delete` | ShutdownPolicyDelete deletes the Sandbox when expired.<br /> |
| `Retain` | ShutdownPolicyRetain keeps the Sandbox when expired (Status will show Expired).<br /> |



## extensions.agents.x-k8s.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types
- [SandboxClaim](#sandboxclaim)
- [SandboxTemplate](#sandboxtemplate)
- [SandboxWarmPool](#sandboxwarmpool)



#### Lifecycle



Lifecycle defines the lifecycle management for the SandboxClaim.



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#time-v1-meta)_ | ShutdownTime is the absolute time when the SandboxClaim expires.<br />This time governs the lifecycle of the claim. It is not propagated to the<br />underlying Sandbox. Instead, the SandboxClaim controller enforces this<br />expiration by deleting the Sandbox resources when the time is reached.<br />If this field is omitted or set to nil, the SandboxClaim itself won't expire.<br />This implies unsetting a Sandbox's ShutdownTime via SandboxClaim isn't supported. |  | Format: date-time <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | ShutdownPolicy determines the behavior when the SandboxClaim expires. | Retain | Enum: [Delete Retain] <br /> |


#### NetworkPolicySpec



NetworkPolicySpec defines the desired state of the NetworkPolicy.



_Appears in:_
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ingress` _[NetworkPolicyIngressRule](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#networkpolicyingressrule-v1-networking) array_ | Ingress is a list of ingress rules to be applied to the sandbox.<br />Traffic is allowed to the sandbox if it matches at least one rule.<br />If this list is empty, all ingress traffic is blocked (Default Deny). |  |  |
| `egress` _[NetworkPolicyEgressRule](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#networkpolicyegressrule-v1-networking) array_ | Egress is a list of egress rules to be applied to the sandbox.<br />Traffic is allowed out of the sandbox if it matches at least one rule.<br />If this list is empty, all egress traffic is blocked (Default Deny). |  |  |


#### SandboxClaim



SandboxClaim is the Schema for the sandbox Claim API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxClaim` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SandboxClaimSpec](#sandboxclaimspec)_ | spec defines the desired state of Sandbox |  |  |


#### SandboxClaimSpec



SandboxClaimSpec defines the desired state of Sandbox



_Appears in:_
- [SandboxClaim](#sandboxclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sandboxTemplateRef` _[SandboxTemplateRef](#sandboxtemplateref)_ | SandboxTemplateRefName - name of the SandboxTemplate to be used for creating a Sandbox |  | Required: \{\} <br /> |
| `lifecycle` _[Lifecycle](#lifecycle)_ | Lifecycle defines when and how the SandboxClaim should be shut down. |  |  |




#### SandboxStatus







_Appears in:_
- [SandboxClaimStatus](#sandboxclaimstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `Name` _string_ | SandboxName is the name of the Sandbox created from this claim |  |  |


#### SandboxTemplate



SandboxTemplate is the Schema for the sandboxe template API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxTemplate` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SandboxTemplateSpec](#sandboxtemplatespec)_ | spec defines the desired state of Sandbox |  |  |


#### SandboxTemplateRef



SandboxTemmplateRef references a SandboxTemplate



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)
- [SandboxWarmPoolSpec](#sandboxwarmpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the SandboxTemplate |  | Required: \{\} <br /> |


#### SandboxTemplateSpec



SandboxTemplateSpec defines the desired state of Sandbox



_Appears in:_
- [SandboxTemplate](#sandboxtemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | template is the object that describes the pod spec that will be used to create<br />an agent sandbox.<br />If AutomountServiceAccountToken is not specified in the PodSpec, it defaults to false<br />to ensure a secure-by-default environment. |  | Required: \{\} <br /> |
| `networkPolicy` _[NetworkPolicySpec](#networkpolicyspec)_ | NetworkPolicy defines the network policy to be applied to the sandboxes<br />created from this template.<br />NOTE: This is a restricted subset of the standard Kubernetes NetworkPolicySpec.<br />Fields like 'PodSelector' and 'PolicyTypes' are intentionally excluded because<br />they are managed by the controller to ensure strict isolation and default-deny posture.<br />WARNING: This policy enforces a strict "Default Deny" ingress posture.<br />If your Pod uses sidecars (e.g., Istio proxy, monitoring agents) that listen<br />on their own ports, the NetworkPolicy will BLOCK traffic to them by default.<br />You MUST explicitly allow traffic to these sidecar ports using 'Ingress',<br />otherwise the sidecars may fail health checks. |  |  |




#### SandboxWarmPool



SandboxWarmPool is the Schema for the sandboxwarmpools API





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxWarmPool` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SandboxWarmPoolSpec](#sandboxwarmpoolspec)_ | spec defines the desired state of SandboxWarmPool |  |  |


#### SandboxWarmPoolSpec



SandboxWarmPoolSpec defines the desired state of SandboxWarmPool



_Appears in:_
- [SandboxWarmPool](#sandboxwarmpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | Replicas is the desired number of sandboxes in the pool.<br />This field is controlled by an HPA if specified. |  | Minimum: 0 <br />Required: \{\} <br /> |
| `sandboxTemplateRef` _[SandboxTemplateRef](#sandboxtemplateref)_ | sandboxTemplateRef - name of the SandboxTemplate to be used for creating a Sandbox |  | Required: \{\} <br /> |




#### ShutdownPolicy

_Underlying type:_ _string_

ShutdownPolicy describes the policy for shutting down the underlying Sandbox when the SandboxClaim expires.

_Validation:_
- Enum: [Delete Retain]

_Appears in:_
- [Lifecycle](#lifecycle)

| Field | Description |
| --- | --- |
| `Delete` | ShutdownPolicyDelete deletes the SandboxClaim (and cascadingly the Sandbox) when expired.<br /> |
| `Retain` | ShutdownPolicyRetain keeps the SandboxClaim when expired (Status will show Expired).<br />The underlying SandboxClaim resources (Sandbox, Pod, Service) are deleted to save resources,<br />but the SandboxClaim object itself remains.<br /> |
