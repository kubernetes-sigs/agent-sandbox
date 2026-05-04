# API Reference

## Packages
- [agents.x-k8s.io/v1alpha1](#agentsx-k8siov1alpha1)
- [extensions.agents.x-k8s.io/v1alpha1](#extensionsagentsx-k8siov1alpha1)


## agents.x-k8s.io/v1alpha1

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group


Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types
- [Sandbox](#sandbox)





#### EmbeddedObjectMetadata







_Appears in:_
- [PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name must be unique within a namespace. Is required when creating resources, although<br />some resources may allow a client to request the generation of an appropriate name<br />automatically. Name is primarily intended for creation idempotence and configuration<br />definition.<br />Cannot be updated.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names#names |  | Optional: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | labels defines the map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  | Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  | Optional: \{\} <br /> |


#### Lifecycle



Lifecycle defines the lifecycle management for the Sandbox.



_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | shutdownTime is the absolute time when the sandbox expires. |  | Format: date-time <br />Optional: \{\} <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | shutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.<br />Underlying resources(Pods, Services) are always deleted on expiry. | Retain | Enum: [Delete Retain] <br />Optional: \{\} <br /> |


#### PersistentVolumeClaimTemplate







_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `metadata` _[EmbeddedObjectMetadata](#embeddedobjectmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[PersistentVolumeClaimSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#persistentvolumeclaimspec-v1-core)_ | spec is the PVC's spec |  | Required: \{\} <br /> |


#### PodMetadata







_Appears in:_
- [PodTemplate](#podtemplate)
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `labels` _object (keys:string, values:string)_ | labels defines the map of string keys and values that can be used to organize and categorize<br />(scope and select) objects. May match selectors of replication controllers<br />and services.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels |  | Optional: \{\} <br /> |
| `annotations` _object (keys:string, values:string)_ | annotations is an unstructured key value map stored with a resource that may be<br />set by external tools to store and retrieve arbitrary metadata. They are not<br />queryable and should be preserved when modifying objects.<br />More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations |  | Optional: \{\} <br /> |


#### PodTemplate







_Appears in:_
- [SandboxSpec](#sandboxspec)
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `spec` _[PodSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#podspec-v1-core)_ | spec is the Pod's spec |  | Required: \{\} <br /> |
| `metadata` _[PodMetadata](#podmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |


#### Sandbox



Sandbox is the Schema for the sandboxes API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `Sandbox` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxSpec](#sandboxspec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |
| `status` _[SandboxStatus](#sandboxstatus)_ | status defines the observed state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxSpec



SandboxSpec defines the desired state of Sandbox.



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate describes the pod spec that will be used to create an agent sandbox. |  | Required: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.<br />Every claim in this list must have at least one matching access mode with a provisioner volume. |  | Optional: \{\} <br /> |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | shutdownTime is the absolute time when the sandbox expires. |  | Format: date-time <br />Optional: \{\} <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | shutdownPolicy determines if the Sandbox resource itself should be deleted when it expires.<br />Underlying resources(Pods, Services) are always deleted on expiry. | Retain | Enum: [Delete Retain] <br />Optional: \{\} <br /> |
| `replicas` _integer_ | replicas is the number of desired replicas.<br />The only allowed values are 0 and 1.<br />Defaults to 1. | 1 | Maximum: 1 <br />Minimum: 0 <br />Optional: \{\} <br /> |


#### SandboxStatus



SandboxStatus defines the observed state of Sandbox.



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `serviceFQDN` _string_ | serviceFQDN that is valid for default cluster settings<br />The domain defaults to cluster.local but is configurable via the controller's --cluster-domain flag. |  | Optional: \{\} <br /> |
| `service` _string_ | service is a sandbox-example |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions defines the status conditions array |  | Optional: \{\} <br /> |
| `replicas` _integer_ | replicas is the number of actual replicas. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `selector` _string_ | selector is the label selector for pods. |  | Optional: \{\} <br /> |
| `podIPs` _string array_ | podIPs are the IP addresses of the underlying pod.<br />A pod may have multiple IPs in dual-stack clusters. |  | Optional: \{\} <br /> |


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

Package v1alpha1 contains API Schema definitions for the extensions v1alpha1 API group

Package v1alpha1 contains API Schema definitions for the agents v1alpha1 API group.

### Resource Types
- [SandboxClaim](#sandboxclaim)
- [SandboxTemplate](#sandboxtemplate)
- [SandboxWarmPool](#sandboxwarmpool)



#### EnvVar



EnvVar represents a custom environment variable key-value pair.



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the environment variable. |  | Required: \{\} <br /> |
| `value` _string_ | value of the environment variable. |  | Required: \{\} <br /> |
| `containerName` _string_ | containerName specifies the target container for the environment variable.<br />If not specified, it defaults to the first container defined in the template. |  | Optional: \{\} <br /> |


#### EnvVarsInjectionPolicy

_Underlying type:_ _string_

EnvVarsInjectionPolicy defines whether a SandboxClaim is allowed to inject or override environment variables.



_Appears in:_
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description |
| --- | --- |
| `Allowed` | EnvVarsInjectionPolicyAllowed allows a SandboxClaim to inject new environment variables, but not override existing ones.<br /> |
| `Overrides` | EnvVarsInjectionPolicyOverrides allows a SandboxClaim to inject new and override existing environment variables.<br /> |
| `Disallowed` | EnvVarsInjectionPolicyDisallowed prevents a SandboxClaim from injecting any environment variables.<br /> |


#### Lifecycle



Lifecycle defines the lifecycle management for the SandboxClaim.



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | shutdownTime is the absolute time when the SandboxClaim expires.<br />This time governs the lifecycle of the claim. It is not propagated to the<br />underlying Sandbox. Instead, the SandboxClaim controller enforces this<br />expiration by deleting the Sandbox resources when the time is reached.<br />If this field is omitted or set to nil, the SandboxClaim itself won't expire.<br />This implies unsetting a Sandbox's ShutdownTime via SandboxClaim isn't supported. |  | Format: date-time <br />Optional: \{\} <br /> |
| `ttlSecondsAfterFinished` _integer_ | ttlSecondsAfterFinished limits how long a finished claim is retained.<br />The timer starts from the mirrored Finished condition's LastTransitionTime. |  | Minimum: 0 <br />Optional: \{\} <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | shutdownPolicy determines the behavior when the SandboxClaim expires. | Retain | Enum: [Delete DeleteForeground Retain] <br />Optional: \{\} <br /> |


#### NetworkPolicyManagement

_Underlying type:_ _string_

NetworkPolicyManagement defines whether the controller automatically generates
and manages a shared NetworkPolicy for this template.



_Appears in:_
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description |
| --- | --- |
| `Managed` | NetworkPolicyManagementManaged means the controller will ensure a shared NetworkPolicy exists.<br />This shared NetworkPolicy will be a user provide one or a default controller created policy.<br />This is the default behavior if the field is omitted.<br /> |
| `Unmanaged` | NetworkPolicyManagementUnmanaged means the controller will skip NetworkPolicy<br />creation entirely, allowing external systems (like Cilium) to manage networking.<br /> |


#### NetworkPolicySpec



NetworkPolicySpec defines the desired state of the NetworkPolicy.



_Appears in:_
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ingress` _[NetworkPolicyIngressRule](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#networkpolicyingressrule-v1-networking) array_ | ingress is a list of ingress rules to be applied to the sandbox.<br />Traffic is allowed to the sandbox if it matches at least one rule.<br />If this list is empty, all ingress traffic is blocked (Default Deny). |  | Optional: \{\} <br /> |
| `egress` _[NetworkPolicyEgressRule](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#networkpolicyegressrule-v1-networking) array_ | egress is a list of egress rules to be applied to the sandbox.<br />Traffic is allowed out of the sandbox if it matches at least one rule.<br />If this list is empty, all egress traffic is blocked (Default Deny). |  | Optional: \{\} <br /> |


#### SandboxClaim



SandboxClaim is the Schema for the sandbox Claim API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxClaim` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxClaimSpec](#sandboxclaimspec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |
| `status` _[SandboxClaimStatus](#sandboxclaimstatus)_ | status defines the observed state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxClaimSpec



SandboxClaimSpec defines the desired state of Sandbox.



_Appears in:_
- [SandboxClaim](#sandboxclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sandboxTemplateRef` _[SandboxTemplateRef](#sandboxtemplateref)_ | sandboxTemplateRef defines the name of the SandboxTemplate to be used for creating a Sandbox. |  | Required: \{\} <br /> |
| `lifecycle` _[Lifecycle](#lifecycle)_ | lifecycle defines when and how the SandboxClaim should be shut down. |  | Optional: \{\} <br /> |
| `warmpool` _[WarmPoolPolicy](#warmpoolpolicy)_ | warmpool specifies the warm pool policy for sandbox adoption.<br />- "none": Do not use any warm pool, always create fresh sandboxes<br />- "default": Use default behavior, select from all matching warm pools (default)<br />- A warm pool name: Select only from the specified warm pool (e.g., "fast-pool", "secure-pool") | default | Optional: \{\} <br /> |
| `additionalPodMetadata` _[PodMetadata](#podmetadata)_ | additionalPodMetadata defines the labels and annotations to be propagated to the Sandbox Pod.<br />Label values are limited to 63 characters and must match Kubernetes label value patterns. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](#envvar) array_ | env is a list of environment variables to inject into the sandbox |  | Optional: \{\} <br /> |


#### SandboxClaimStatus



SandboxClaimStatus defines the observed state of Sandbox.



_Appears in:_
- [SandboxClaim](#sandboxclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions represent the latest available observations of a Sandbox's current state. |  | Optional: \{\} <br /> |
| `sandbox` _[SandboxStatus](#sandboxstatus)_ | sandbox defines the state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxStatus







_Appears in:_
- [SandboxClaimStatus](#sandboxclaimstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name is the name of the Sandbox created from this claim |  | Optional: \{\} <br /> |
| `podIPs` _string array_ | podIPs are the IP addresses of the underlying pod.<br />A pod may have multiple IPs in dual-stack clusters. |  | Optional: \{\} <br /> |


#### SandboxTemplate



SandboxTemplate is the Schema for the sandbox template API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxTemplate` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxTemplateSpec](#sandboxtemplatespec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |
| `status` _[SandboxTemplateStatus](#sandboxtemplatestatus)_ | status defines the observed state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxTemplateRef



SandboxTemplateRef references a SandboxTemplate.



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)
- [SandboxWarmPoolSpec](#sandboxwarmpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the SandboxTemplate |  | Required: \{\} <br /> |


#### SandboxTemplateSpec



SandboxTemplateSpec defines the desired state of Sandbox.



_Appears in:_
- [SandboxTemplate](#sandboxtemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate defines the object template that describes the pod spec that will be used to create<br />an agent sandbox.<br />If AutomountServiceAccountToken is not specified in the PodSpec, it defaults to false<br />to ensure a secure-by-default environment. |  | Required: \{\} <br /> |
| `networkPolicy` _[NetworkPolicySpec](#networkpolicyspec)_ | networkPolicy defines the network policy to be applied to the sandboxes<br />created from this template. A single shared NetworkPolicy is created per Template.<br />Behavior is dictated by the NetworkPolicyManagement field:<br />- If Management is "Unmanaged": This field is completely ignored.<br />- If Management is "Managed" (default) and this field is omitted (nil): The controller<br />  automatically applies a strict Secure Default policy:<br />    * Ingress: Allow traffic only from the Sandbox Router.<br />    * Egress: Allow Public Internet only. Blocks internal IPs (RFC1918), Metadata Server, etc.<br />- If Management is "Managed" and this field is provided: The controller applies your custom rules.<br />Update Behavior:<br />Because the NetworkPolicy is shared at the template level, any updates to these rules<br />will be applied to the single shared policy object. The underlying Kubernetes CNI will then<br />dynamically enforce the updated rules across all existing and future sandboxes<br />referencing this template.<br />NOTE: This is a restricted subset of the standard Kubernetes NetworkPolicySpec.<br />Fields like 'PodSelector' and 'PolicyTypes' are intentionally excluded because<br />they are managed by the controller to ensure strict isolation and default-deny posture.<br />WARNING: This policy enforces a strict "Default Deny" ingress posture.<br />If your Pod uses sidecars (e.g., Istio proxy, monitoring agents) that listen<br />on their own ports, the NetworkPolicy will BLOCK traffic to them by default.<br />You MUST explicitly allow traffic to these sidecar ports using 'Ingress',<br />otherwise the sidecars may fail health checks. |  | Optional: \{\} <br /> |
| `networkPolicyManagement` _[NetworkPolicyManagement](#networkpolicymanagement)_ | networkPolicyManagement defines whether the controller manages the NetworkPolicy.<br />Valid values are "Managed" (default) or "Unmanaged". | Managed | Enum: [Managed Unmanaged] <br />Optional: \{\} <br /> |
| `envVarsInjectionPolicy` _[EnvVarsInjectionPolicy](#envvarsinjectionpolicy)_ | envVarsInjectionPolicy allows a SandboxClaim to inject or override environment variables defined in the template.<br />If set to Disallowed, the SandboxClaim will be rejected if it specifies any environment variables. | Disallowed | Enum: [Allowed Overrides Disallowed] <br />Optional: \{\} <br /> |


#### SandboxTemplateStatus



SandboxTemplateStatus defines the observed state of Sandbox.



_Appears in:_
- [SandboxTemplate](#sandboxtemplate)



#### SandboxWarmPool



SandboxWarmPool is the Schema for the sandboxwarmpools API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1alpha1` | | |
| `kind` _string_ | `SandboxWarmPool` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxWarmPoolSpec](#sandboxwarmpoolspec)_ | spec defines the desired state of SandboxWarmPool |  | Required: \{\} <br /> |
| `status` _[SandboxWarmPoolStatus](#sandboxwarmpoolstatus)_ | status defines the observed state of SandboxWarmPool |  | Optional: \{\} <br /> |


#### SandboxWarmPoolSpec



SandboxWarmPoolSpec defines the desired state of SandboxWarmPool.



_Appears in:_
- [SandboxWarmPool](#sandboxwarmpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | replicas is the desired number of sandboxes in the pool.<br />This field is controlled by an HPA if specified. |  | Minimum: 0 <br />Required: \{\} <br /> |
| `sandboxTemplateRef` _[SandboxTemplateRef](#sandboxtemplateref)_ | sandboxTemplateRef - name of the SandboxTemplate to be used for creating a Sandbox<br />Warning: Any change to the json tag "sandboxTemplateRef" must be synchronized with the TemplateRefField constant. |  | Required: \{\} <br /> |
| `updateStrategy` _[SandboxWarmPoolUpdateStrategy](#sandboxwarmpoolupdatestrategy)_ | updateStrategy - strategy for updating the SandboxWarmPool pods based on sandboxTemplateRef name change or underlying template changes |  | Optional: \{\} <br /> |


#### SandboxWarmPoolStatus



SandboxWarmPoolStatus defines the observed state of SandboxWarmPool.



_Appears in:_
- [SandboxWarmPool](#sandboxwarmpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | replicas is the total number of sandboxes in the pool. |  | Optional: \{\} <br /> |
| `readyReplicas` _integer_ | readyReplicas is the total number of sandboxes in the pool that are in a ready state. |  | Optional: \{\} <br /> |
| `selector` _string_ | selector is the label selector used to find the pods in the pool. |  | Optional: \{\} <br /> |


#### SandboxWarmPoolUpdateStrategy



SandboxWarmPoolUpdateStrategy defines the update strategy for the SandboxWarmPool.



_Appears in:_
- [SandboxWarmPoolSpec](#sandboxwarmpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[SandboxWarmPoolUpdateStrategyType](#sandboxwarmpoolupdatestrategytype)_ | type indicates the type of the SandboxWarmPoolUpdateStrategy.<br />Default is OnReplenish. | OnReplenish | Enum: [Recreate OnReplenish] <br />Optional: \{\} <br /> |


#### SandboxWarmPoolUpdateStrategyType

_Underlying type:_ _string_

SandboxWarmPoolUpdateStrategyType is a string enumeration type that enumerates
all possible update strategies for the SandboxWarmPool controller.

_Validation:_
- Enum: [Recreate OnReplenish]

_Appears in:_
- [SandboxWarmPoolUpdateStrategy](#sandboxwarmpoolupdatestrategy)

| Field | Description |
| --- | --- |
| `Recreate` | RecreateSandboxWarmPoolUpdateStrategyType indicates that stale pods are deleted immediately to ensure the pool only contains fresh pods.<br />Note: This applies to PodTemplate spec changes only. Changes to annotations or labels in the template do not trigger recreate.<br /> |
| `OnReplenish` | OnReplenishSandboxWarmPoolUpdateStrategyType indicates that stale pods are only replaced when they are manually deleted or when these stale pods are adopted by sandboxclaims and hence replaced by fresh pods.<br /> |


#### ShutdownPolicy

_Underlying type:_ _string_

ShutdownPolicy describes the policy for shutting down the underlying Sandbox when the SandboxClaim expires.

_Validation:_
- Enum: [Delete DeleteForeground Retain]

_Appears in:_
- [Lifecycle](#lifecycle)

| Field | Description |
| --- | --- |
| `Delete` | ShutdownPolicyDelete deletes the SandboxClaim (and cascadingly the Sandbox) when expired.<br /> |
| `DeleteForeground` | ShutdownPolicyDeleteForeground deletes the SandboxClaim when expired using foreground<br />cascade deletion. The claim remains in the API (with a deletionTimestamp) until its<br />underlying Sandbox and Pod are fully terminated. This allows external systems to observe<br />shutdown progress by checking whether the claim still exists.<br /> |
| `Retain` | ShutdownPolicyRetain keeps the SandboxClaim when expired (Status will show Expired).<br />The underlying SandboxClaim resources (Sandbox, Pod, Service) are deleted to save resources,<br />but the SandboxClaim object itself remains.<br /> |


#### WarmPoolPolicy

_Underlying type:_ _string_

WarmPoolPolicy describes the policy for using warm pools.
It can be one of the following:
  - "none": Do not use any warm pool, always create fresh sandboxes
  - "default": Select from all available warm pools that match the template (default)
  - A warm pool name: Select only from the specified warm pool (e.g., "fast-pool", "secure-pool")



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description |
| --- | --- |
| `none` | WarmPoolPolicyNone indicates that no warm pool should be used.<br />A fresh sandbox will always be created.<br /> |
| `default` | WarmPoolPolicyDefault indicates the default behavior: select from all<br />available warm pools that match the template. This is the default behavior<br />if warmpool is not specified.<br /> |


