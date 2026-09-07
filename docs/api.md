# API Reference

## Packages
- [agents.x-k8s.io/v1beta1](#agentsx-k8siov1beta1)
- [extensions.agents.x-k8s.io/v1beta1](#extensionsagentsx-k8siov1beta1)


## agents.x-k8s.io/v1beta1

Package v1beta1 contains API Schema definitions for the agents v1beta1 API group


Package v1beta1 contains API Schema definitions for the agents v1beta1 API group.

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
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | shutdownTime is the absolute time at which the Sandbox expires. When the current<br />time reaches shutdownTime, the controller tears down the underlying resources<br />(Pod and Service) and then applies shutdownPolicy to the Sandbox object itself.<br />If unset, the Sandbox never expires and lives until it is explicitly deleted. |  | Format: date-time <br />Optional: \{\} <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | shutdownPolicy determines what happens to the Sandbox object itself when it expires<br />(i.e. when shutdownTime is reached). The underlying resources (Pod, Service) are<br />always deleted on expiry regardless of this policy; shutdownPolicy governs only the<br />Sandbox object:<br />  - Retain (default): the Sandbox object is kept after its resources are torn down.<br />    Its live status fields are cleared and a Ready=False condition with reason<br />    SandboxExpired is set so the expiry is observable.<br />  - Delete: the Sandbox object is deleted once its underlying resources are removed.<br />This field has no effect while shutdownTime is unset, since the Sandbox never expires. | Retain | Enum: [Delete Retain] <br />Optional: \{\} <br /> |


#### PersistentVolumeClaimTemplate







_Appears in:_
- [SandboxBlueprint](#sandboxblueprint)
- [SandboxClaimSpec](#sandboxclaimspec)
- [SandboxSpec](#sandboxspec)
- [SandboxTemplateSpec](#sandboxtemplatespec)

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
- [SandboxBlueprint](#sandboxblueprint)
- [SandboxSpec](#sandboxspec)
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `spec` _[PodSpec](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#podspec-v1-core)_ | spec is the Pod's spec |  | Required: \{\} <br /> |
| `metadata` _[PodMetadata](#podmetadata)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |


#### RuntimeActivationVerification



RuntimeActivationVerification is owned exclusively by the GKR readiness controller.
A claim must match the complete current verification and its validity deadline;
a Ready condition or receipt for the same Pod under another attempt is insufficient.



_Appears in:_
- [SandboxStatus](#sandboxstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `attemptID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | attemptID identifies the committed adoption being verified. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `claimUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | claimUID identifies the current claimant. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `podUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | podUID identifies the retained Pod verified by the runtime controller. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `nodeUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | nodeUID identifies the Node whose current key verified the receipt. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `containerID` _string_ | containerID identifies the retained application container. |  | MaxLength: 256 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `taskStartTime` _integer_ | taskStartTime distinguishes the retained task from PID reuse or restart. |  | Minimum: 1 <br />Required: \{\} <br /> |
| `runtimeIncarnation` _string_ | runtimeIncarnation identifies the native runtime instance that verified execution. |  | MaxLength: 128 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `targetActivationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | targetActivationID identifies the current successor binding. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `receiptDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | receiptDigest identifies the current verified target receipt. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `contextDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | contextDigest binds the receipt to this exact adoption context. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `commitDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | commitDigest identifies the confirmed runtime release observation. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `policyEpoch` _integer_ | policyEpoch is the exact prepared policy epoch, not an API resource version. |  | Minimum: 0 <br />Required: \{\} <br /> |
| `validUntil` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | validUntil bounds the current observational evidence, not the historical commit. |  | Required: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions report the current verification result. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### RuntimeAdoptionConsumption



RuntimeAdoptionConsumption permanently retires a sandbox's first-claim eligibility.
It is distinct from the node's consumption of release authorization. A failed
attempt that may have held execution also consumes eligibility.



_Appears in:_
- [RuntimeAdoptionStatus](#runtimeadoptionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `attemptID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | attemptID identifies the attempt that retired the sandbox from the pool. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `claimUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | claimUID identifies that attempt's claim even after the claim is deleted. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `consumedTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | consumedTime records when first-claim eligibility was retired. |  | Required: \{\} <br /> |


#### RuntimeAdoptionDigest

_Underlying type:_ _string_

RuntimeAdoptionDigest identifies a domain-separated canonical SHA-256 payload.

_Validation:_
- MaxLength: 71
- MinLength: 71
- Pattern: `^sha256:[0-9a-f]{64}$`

_Appears in:_
- [RuntimeActivationVerification](#runtimeactivationverification)
- [RuntimeAdoptionGrant](#runtimeadoptiongrant)
- [RuntimeAdoptionInitialization](#runtimeadoptioninitialization)
- [RuntimeAdoptionReservation](#runtimeadoptionreservation)
- [RuntimeAdoptionStatus](#runtimeadoptionstatus)
- [SandboxClaimRuntimeAdoptionStatus](#sandboxclaimruntimeadoptionstatus)



#### RuntimeAdoptionGrant



RuntimeAdoptionGrant records the API-side authorization for one held transfer receipt.
The node must independently verify it and durably consume its signed grant once.



_Appears in:_
- [RuntimeAdoptionStatus](#runtimeadoptionstatus)
- [SandboxClaimRuntimeAdoptionStatus](#sandboxclaimruntimeadoptionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `contextDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | contextDigest identifies the immutable prepared adoption context. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `receiptDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | receiptDigest identifies the held target receipt authorized for release. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `expiresAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | expiresAt must not exceed the reservation, preparation, or receipt deadline. |  | Required: \{\} <br /> |
| `grantDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | grantDigest identifies the node-signed start grant after it has been observed. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |


#### RuntimeAdoptionID

_Underlying type:_ _string_

RuntimeAdoptionID identifies an initialization, activation, or durable adoption attempt.
IDs must be generated by a trusted controller; their syntax does not establish authority.

_Validation:_
- MaxLength: 128
- MinLength: 16
- Pattern: `^[A-Za-z0-9._-]+$`

_Appears in:_
- [RuntimeActivationVerification](#runtimeactivationverification)
- [RuntimeAdoptionConsumption](#runtimeadoptionconsumption)
- [RuntimeAdoptionInitialization](#runtimeadoptioninitialization)
- [RuntimeAdoptionReservation](#runtimeadoptionreservation)



#### RuntimeAdoptionInitialization



RuntimeAdoptionInitialization references authenticated strict pool initialization.
Only the signed envelope and matching live runtime state establish eligibility;
the summary fields are not independent authorization.



_Appears in:_
- [RuntimeAdoptionStatus](#runtimeadoptionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `initializationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | initializationID identifies the original initialization observation. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `sourceActivationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | sourceActivationID identifies the strict cold activation used to initialize the pool member. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `poolUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | poolUID identifies the pool that created the unused sandbox. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `templateUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | templateUID identifies the template used for initialization. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `blueprintDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | blueprintDigest binds the versioned immutable workload projection. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `receiptDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | receiptDigest identifies the original strict cold activation receipt. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `envelope` _integer array_ | envelope contains the canonical signed initialization envelope, encoded as base64<br />in JSON. Opaque bytes preserve the signed encoding through API-server storage. |  | MaxLength: 262144 <br />MinLength: 1 <br />Required: \{\} <br /> |


#### RuntimeAdoptionObjectReference



RuntimeAdoptionObjectReference keeps a UID-bound locator through deletion and recovery.



_Appears in:_
- [SandboxClaimRuntimeAdoptionStatus](#sandboxclaimruntimeadoptionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name locates the referenced object. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `uid` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | uid distinguishes the original object from a replacement with the same name. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |


#### RuntimeAdoptionReservation



RuntimeAdoptionReservation binds one live claim to one initialized sandbox.
Both the claim and sandbox must contain this exact reservation before a runtime
may act. Names locate objects; UIDs, authenticated writers, and live readback
establish their identities. Resource versions are optimistic-write fences and
are deliberately absent from this immutable protocol context.



_Appears in:_
- [RuntimeAdoptionStatus](#runtimeadoptionstatus)
- [SandboxClaimRuntimeAdoptionStatus](#sandboxclaimruntimeadoptionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `wireVersion` _string_ | wireVersion identifies the canonical reservation encoding. |  | Enum: [runtime.gatekeeper.sh/same-policy-adoption/v1alpha1] <br />Required: \{\} <br /> |
| `attemptID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | attemptID identifies this attempt across retries and controller restarts. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `initializationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | initializationID identifies the verified first-claim-only pool initialization. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `sourceActivationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | sourceActivationID is the expected predecessor that currently owns the execution. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `targetActivationID` _[RuntimeAdoptionID](#runtimeadoptionid)_ | targetActivationID is the only permitted successor for this attempt. |  | MaxLength: 128 <br />MinLength: 16 <br />Pattern: `^[A-Za-z0-9._-]+$` <br />Required: \{\} <br /> |
| `namespace` _string_ | namespace is shared by the claim, sandbox, pool, and template. |  | MaxLength: 63 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `namespaceUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | namespaceUID prevents a recreated namespace from reusing this reservation. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `claimName` _string_ | claimName locates the claiming SandboxClaim. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `claimUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | claimUID is the identity of the claiming SandboxClaim. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `claimGeneration` _integer_ | claimGeneration records the claim spec accepted for the attempt. |  | Minimum: 1 <br />Required: \{\} <br /> |
| `claimIntentDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | claimIntentDigest also covers metadata inputs that do not increment generation. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |
| `sandboxUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | sandboxUID is the identity of the exclusively reserved Sandbox. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `poolUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | poolUID is the identity of the original SandboxWarmPool. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `templateUID` _[UID](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#uid-types-pkg)_ | templateUID is the identity of the initialized SandboxTemplate. |  | MaxLength: 128 <br />MinLength: 1 <br />Type: string <br />Required: \{\} <br /> |
| `expiresAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | expiresAt bounds new authorization; expiry never returns uncertain execution to the pool. |  | Required: \{\} <br /> |


#### RuntimeAdoptionStatus



RuntimeAdoptionStatus is owned by the trusted claim controller.
Protected fields require authenticated writers and admission checks; their
presence alone does not enable warm execution or authorize any runtime action.



_Appears in:_
- [SandboxStatus](#sandboxstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `initialization` _[RuntimeAdoptionInitialization](#runtimeadoptioninitialization)_ | initialization retains the first-use origin for the lifetime of the sandbox. |  | Optional: \{\} <br /> |
| `reservation` _[RuntimeAdoptionReservation](#runtimeadoptionreservation)_ | reservation is exclusive and survives cancellation until the node proves a terminal result. |  | Optional: \{\} <br /> |
| `holdEvidenceDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | holdEvidenceDigest records the authenticated hold before ownership or metadata changes. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `targetMetadataDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | targetMetadataDigest identifies the final API-server-observed target metadata. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `grant` _[RuntimeAdoptionGrant](#runtimeadoptiongrant)_ | grant records the accepted API-side release authorization. |  | Optional: \{\} <br /> |
| `commitDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | commitDigest identifies the immutable, confirmed runtime release result. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `terminationRequestedTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | terminationRequestedTime requests owner-aware cleanup without erasing the attempt.<br />Once set, no new release authorization may be issued for this reservation. |  | Optional: \{\} <br /> |
| `consumed` _[RuntimeAdoptionConsumption](#runtimeadoptionconsumption)_ | consumed permanently prevents this sandbox from serving another first claim. |  | Optional: \{\} <br /> |
| `terminalEvidenceDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | terminalEvidenceDigest identifies the retained node observation used for finalization. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions record observed adoption progress without replacing the durable identity. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### Sandbox



Sandbox is the Schema for the sandboxes API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `agents.x-k8s.io/v1beta1` | | |
| `kind` _string_ | `Sandbox` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxSpec](#sandboxspec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |
| `status` _[SandboxStatus](#sandboxstatus)_ | status defines the observed state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxBlueprint



SandboxBlueprint defines the configuration shared between Sandbox and SandboxTemplate.
It deliberately excludes runtime-only fields (operatingMode, lifecycle).



_Appears in:_
- [SandboxSpec](#sandboxspec)
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate describes the pod that will be created in the sandbox.<br />Note: When provisioned via a SandboxTemplate (such as by a SandboxClaim or SandboxWarmPool),<br />if AutomountServiceAccountToken is not specified in the PodSpec, the controller defaults it<br />to false to ensure a secure-by-default environment. |  | Required: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.<br />When creating a sandbox, PVCs will be created from these templates.<br />Every claim in this list must have at least one matching access mode with a provisioner volume.<br />NOTE: This list is atomic. Updates to this field will replace the entire list rather than merging with existing entries. |  | Optional: \{\} <br /> |
| `service` _boolean_ | service controls whether the controller should automatically create a<br />headless Service for the Sandbox workload.<br />When unset, the controller preserves existing Services for backward<br />compatibility but does not create new ones. Set to true to enable or false<br />to explicitly disable and remove the Service. |  | Optional: \{\} <br /> |


#### SandboxOperatingMode

_Underlying type:_ _string_

SandboxOperatingMode defines the desired operational state of the Sandbox.

It expresses intent ("running" vs. "suspended"), not observed status; whether the
Sandbox has actually reached that state is reported by conditions (see
SandboxConditionReady and SandboxConditionSuspended).

_Validation:_
- Enum: [Running Suspended]

_Appears in:_
- [SandboxSpec](#sandboxspec)

| Field | Description |
| --- | --- |
| `Running` | SandboxOperatingModeRunning indicates the Sandbox should be actively running:<br />the controller ensures a backing Pod (and Service, if requested) is created and<br />kept running. This is a desired-state declaration only; observed readiness is<br />reported separately by the Ready condition (see SandboxConditionReady), which<br />stays False until the Pod is actually Running and Ready.<br /> |
| `Suspended` | SandboxOperatingModeSuspended indicates the Sandbox should be suspended: the<br />controller terminates the backing Pod while retaining the Sandbox object and its<br />volumes. Progress of the suspension is reported by the Suspended condition (see<br />SandboxConditionSuspended).<br /> |


#### SandboxSpec



SandboxSpec defines the desired state of Sandbox.
volumeClaimTemplates is immutable after creation.



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate describes the pod that will be created in the sandbox.<br />Note: When provisioned via a SandboxTemplate (such as by a SandboxClaim or SandboxWarmPool),<br />if AutomountServiceAccountToken is not specified in the PodSpec, the controller defaults it<br />to false to ensure a secure-by-default environment. |  | Required: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.<br />When creating a sandbox, PVCs will be created from these templates.<br />Every claim in this list must have at least one matching access mode with a provisioner volume.<br />NOTE: This list is atomic. Updates to this field will replace the entire list rather than merging with existing entries. |  | Optional: \{\} <br /> |
| `service` _boolean_ | service controls whether the controller should automatically create a<br />headless Service for the Sandbox workload.<br />When unset, the controller preserves existing Services for backward<br />compatibility but does not create new ones. Set to true to enable or false<br />to explicitly disable and remove the Service. |  | Optional: \{\} <br /> |
| `shutdownTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | shutdownTime is the absolute time at which the Sandbox expires. When the current<br />time reaches shutdownTime, the controller tears down the underlying resources<br />(Pod and Service) and then applies shutdownPolicy to the Sandbox object itself.<br />If unset, the Sandbox never expires and lives until it is explicitly deleted. |  | Format: date-time <br />Optional: \{\} <br /> |
| `shutdownPolicy` _[ShutdownPolicy](#shutdownpolicy)_ | shutdownPolicy determines what happens to the Sandbox object itself when it expires<br />(i.e. when shutdownTime is reached). The underlying resources (Pod, Service) are<br />always deleted on expiry regardless of this policy; shutdownPolicy governs only the<br />Sandbox object:<br />  - Retain (default): the Sandbox object is kept after its resources are torn down.<br />    Its live status fields are cleared and a Ready=False condition with reason<br />    SandboxExpired is set so the expiry is observable.<br />  - Delete: the Sandbox object is deleted once its underlying resources are removed.<br />This field has no effect while shutdownTime is unset, since the Sandbox never expires. | Retain | Enum: [Delete Retain] <br />Optional: \{\} <br /> |
| `operatingMode` _[SandboxOperatingMode](#sandboxoperatingmode)_ | operatingMode specifies the desired operational state of the Sandbox:<br />  - Running (default): the controller keeps a backing Pod running.<br />  - Suspended: the controller terminates the backing Pod but retains the<br />    Sandbox object and its volumes so it can later be resumed.<br />This field declares intent only. The observed readiness of the Sandbox is<br />reported by the Ready condition, and the progress of a suspension by the<br />Suspended condition; a Sandbox in Running mode is not Ready until its Pod is<br />actually up (see SandboxConditionReady).<br />Defaults to Running if not specified. | Running | Enum: [Running Suspended] <br />Optional: \{\} <br /> |


#### SandboxStatus



SandboxStatus defines the observed state of Sandbox.



_Appears in:_
- [Sandbox](#sandbox)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `runtimeAdoption` _[RuntimeAdoptionStatus](#runtimeadoptionstatus)_ | runtimeAdoption retains controller-owned pool origin and exclusive claim reservation.<br />Core Sandbox reconciliation must preserve it, including after expiry. |  | Optional: \{\} <br /> |
| `runtimeActivationVerification` _[RuntimeActivationVerification](#runtimeactivationverification)_ | runtimeActivationVerification reports current claim-bound GKR evidence.<br />Only the GKR readiness controller may write this field. |  | Optional: \{\} <br /> |
| `serviceFQDN` _string_ | serviceFQDN that is valid for default cluster settings<br />The domain defaults to cluster.local but is configurable via the controller's --cluster-domain flag. |  | Optional: \{\} <br /> |
| `service` _string_ | service is the name of the headless Service created for this Sandbox. It is empty<br />when no Service exists for the Sandbox (for example when spec.service is false, or<br />unset with no pre-existing Service). See serviceFQDN for the fully qualified<br />in-cluster DNS name of this Service. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions defines the status conditions array |  | Optional: \{\} <br /> |
| `selector` _string_ | selector is the label selector for pods. |  | Optional: \{\} <br /> |
| `podIPs` _string array_ | podIPs are the IP addresses of the underlying pod.<br />A pod may have multiple IPs in dual-stack clusters.<br />This field is populated only while a backing pod exists. It is cleared whenever<br />the pod is absent, for example when the Sandbox is suspended<br />(operatingMode: Suspended) or before the pod has been created. |  | Optional: \{\} <br /> |
| `nodeName` _string_ | nodeName is the name of the node where the underlying pod is scheduled.<br />Like podIPs, it is cleared whenever the pod is absent (e.g. while suspended). |  | Optional: \{\} <br /> |


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



## extensions.agents.x-k8s.io/v1beta1

Package v1beta1 contains API Schema definitions for the extensions v1beta1 API group

Package v1beta1 contains API Schema definitions for the extensions.agents v1beta1 API group.

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

_Validation:_
- Enum: [Allowed Overrides Disallowed]

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

_Validation:_
- Enum: [Managed Unmanaged]

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
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1beta1` | | |
| `kind` _string_ | `SandboxClaim` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxClaimSpec](#sandboxclaimspec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |
| `status` _[SandboxClaimStatus](#sandboxclaimstatus)_ | status defines the observed state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxClaimRuntimeAdoptionStatus



SandboxClaimRuntimeAdoptionStatus binds a claim to its reserved sandbox and runtime attempt.
This field requires a trusted controller writer and is not workload-supplied authorization.



_Appears in:_
- [SandboxClaimStatus](#sandboxclaimstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sandboxName` _string_ | sandboxName locates the sandbox whose UID is bound by reservation. |  | MaxLength: 253 <br />MinLength: 1 <br />Required: \{\} <br /> |
| `podRef` _[RuntimeAdoptionObjectReference](#runtimeadoptionobjectreference)_ | podRef records the retained Pod verified before the reservation was made. |  | Required: \{\} <br /> |
| `nodeRef` _[RuntimeAdoptionObjectReference](#runtimeadoptionobjectreference)_ | nodeRef retains the node evidence locator even after Pod deletion. |  | Required: \{\} <br /> |
| `reservation` _[RuntimeAdoptionReservation](#runtimeadoptionreservation)_ | reservation must exactly match the reserved sandbox's protected status. |  | Required: \{\} <br /> |
| `grant` _[RuntimeAdoptionGrant](#runtimeadoptiongrant)_ | grant records a UID/resourceVersion-guarded API-side release authorization. |  | Optional: \{\} <br /> |
| `commitDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | commitDigest identifies the immutable runtime release accepted for this assignment. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `terminationRequestedTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#time-v1-meta)_ | terminationRequestedTime requests runtime cleanup while retaining the attempt. |  | Optional: \{\} <br /> |
| `terminalEvidenceDigest` _[RuntimeAdoptionDigest](#runtimeadoptiondigest)_ | terminalEvidenceDigest identifies the retained node result used for finalization. |  | MaxLength: 71 <br />MinLength: 71 <br />Pattern: `^sha256:[0-9a-f]\{64\}$` <br />Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions report progress for this attempt without clearing its identity. |  | MaxItems: 16 <br />Optional: \{\} <br /> |


#### SandboxClaimSpec



SandboxClaimSpec defines the desired state of Sandbox.



_Appears in:_
- [SandboxClaim](#sandboxclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `warmPoolRef` _[SandboxWarmPoolRef](#sandboxwarmpoolref)_ | warmPoolRef targets the specific pre-warmed infrastructure pool to check out from. |  | Required: \{\} <br /> |
| `lifecycle` _[Lifecycle](#lifecycle)_ | lifecycle defines when and how the SandboxClaim should be shut down. |  | Optional: \{\} <br /> |
| `additionalPodMetadata` _[PodMetadata](#podmetadata)_ | additionalPodMetadata defines the labels and annotations to be propagated to the Sandbox Pod.<br />Label keys must carry a domain prefix from the controller's label-domain allowlist<br />(default: sandbox.users.io, including subdomains), configured via the<br />allowed-label-domains key of the optional agent-sandbox-config ConfigMap.<br />Label values are limited to 63 characters and must match Kubernetes label value patterns.<br />Annotations in restricted system domains are rejected, except cluster-autoscaler.kubernetes.io/safe-to-evict. |  | Optional: \{\} <br /> |
| `env` _[EnvVar](#envvar) array_ | env is a list of environment variables to inject into the sandbox.<br />Please note adding this field means the Sandbox will always be cold-started from the<br />template of the warmpool. |  | Optional: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | volumeClaimTemplates is a list of persistent volume claims to be created for the sandbox.<br />Specifying this field forces a cold start because warm pool pods will not have these volumes. |  | Optional: \{\} <br /> |


#### SandboxClaimStatus



SandboxClaimStatus defines the observed state of Sandbox.



_Appears in:_
- [SandboxClaim](#sandboxclaim)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `runtimeAdoption` _[SandboxClaimRuntimeAdoptionStatus](#sandboxclaimruntimeadoptionstatus)_ | runtimeAdoption records the claim controller's single durable warm-adoption attempt.<br />A stored attempt cannot be replaced while its runtime outcome is uncertain. |  | Optional: \{\} <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#condition-v1-meta) array_ | conditions represent the latest available observations of a Sandbox's current state. |  | Optional: \{\} <br /> |
| `sandbox` _[SandboxStatus](#sandboxstatus)_ | sandbox defines the state of Sandbox |  | Optional: \{\} <br /> |


#### SandboxStatus







_Appears in:_
- [SandboxClaimStatus](#sandboxclaimstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name is the name of the Sandbox created from this claim |  | Optional: \{\} <br /> |
| `podIPs` _string array_ | podIPs are the IP addresses of the underlying pod, mirrored from the backing<br />Sandbox's status. A pod may have multiple IPs in dual-stack clusters.<br />This is populated only while the backing Sandbox has a running pod with assigned<br />IPs; it is cleared whenever the pod is absent (e.g. before the pod has been<br />created or while the Sandbox is suspended). |  | Optional: \{\} <br /> |
| `serviceFQDN` _string_ | serviceFQDN is the in-cluster DNS name of the bound Sandbox's service,<br />mirrored from the Sandbox's status.serviceFQDN so consumers can reach<br />the sandbox from the claim alone. Like name and podIPs, it is eventually<br />consistent: it may lag the Sandbox by a reconcile, and is cleared when<br />the claim loses its sandbox. |  | Optional: \{\} <br /> |


#### SandboxTemplate



SandboxTemplate is the Schema for the sandbox template API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1beta1` | | |
| `kind` _string_ | `SandboxTemplate` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxTemplateSpec](#sandboxtemplatespec)_ | spec defines the desired state of Sandbox |  | Required: \{\} <br /> |


#### SandboxTemplateRef



SandboxTemplateRef references a SandboxTemplate.



_Appears in:_
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
| `podTemplate` _[PodTemplate](#podtemplate)_ | podTemplate describes the pod that will be created in the sandbox.<br />Note: When provisioned via a SandboxTemplate (such as by a SandboxClaim or SandboxWarmPool),<br />if AutomountServiceAccountToken is not specified in the PodSpec, the controller defaults it<br />to false to ensure a secure-by-default environment. |  | Required: \{\} <br /> |
| `volumeClaimTemplates` _[PersistentVolumeClaimTemplate](#persistentvolumeclaimtemplate) array_ | volumeClaimTemplates is a list of claims that the sandbox pod is allowed to reference.<br />When creating a sandbox, PVCs will be created from these templates.<br />Every claim in this list must have at least one matching access mode with a provisioner volume.<br />NOTE: This list is atomic. Updates to this field will replace the entire list rather than merging with existing entries. |  | Optional: \{\} <br /> |
| `service` _boolean_ | service controls whether the controller should automatically create a<br />headless Service for the Sandbox workload.<br />When unset, the controller preserves existing Services for backward<br />compatibility but does not create new ones. Set to true to enable or false<br />to explicitly disable and remove the Service. |  | Optional: \{\} <br /> |
| `networkPolicy` _[NetworkPolicySpec](#networkpolicyspec)_ | networkPolicy defines the network policy to be applied to the sandboxes<br />created from this template. A single shared NetworkPolicy is created per Template.<br />Behavior is dictated by the NetworkPolicyManagement field:<br />- If Management is "Unmanaged": This field is completely ignored.<br />- If Management is "Managed" (default) and this field is omitted (nil): The controller<br />  automatically applies a strict Secure Default policy:<br />    * Ingress: Allow traffic only from the Sandbox Router.<br />    * Egress: Allow Public Internet only. Blocks internal IPs (RFC1918), Metadata Server, etc.<br />- If Management is "Managed" and this field is provided: The controller applies your custom rules.<br />Update Behavior:<br />Because the NetworkPolicy is shared at the template level, any updates to these rules<br />will be applied to the single shared policy object. The underlying Kubernetes CNI will then<br />dynamically enforce the updated rules across all existing and future sandboxes<br />referencing this template.<br />NOTE: This is a restricted subset of the standard Kubernetes NetworkPolicySpec.<br />Fields like 'PodSelector' and 'PolicyTypes' are intentionally excluded because<br />they are managed by the controller to ensure strict isolation and default-deny posture.<br />WARNING: This policy enforces a strict "Default Deny" ingress posture.<br />If your Pod uses sidecars (e.g., Istio proxy, monitoring agents) that listen<br />on their own ports, the NetworkPolicy will BLOCK traffic to them by default.<br />You MUST explicitly allow traffic to these sidecar ports using 'Ingress',<br />otherwise the sidecars may fail health checks. |  | Optional: \{\} <br /> |
| `networkPolicyManagement` _[NetworkPolicyManagement](#networkpolicymanagement)_ | networkPolicyManagement defines whether the controller manages the NetworkPolicy.<br />Valid values are "Managed" (default) or "Unmanaged". | Managed | Enum: [Managed Unmanaged] <br />Optional: \{\} <br /> |
| `envVarsInjectionPolicy` _[EnvVarsInjectionPolicy](#envvarsinjectionpolicy)_ | envVarsInjectionPolicy controls whether a SandboxClaim may set environment variables<br />(spec.env) on sandboxes created from this template:<br />  - Disallowed (default): claims may not set any environment variables; a claim that<br />    specifies spec.env is rejected.<br />  - Allowed: claims may add environment variables whose names are not already defined<br />    in the template, but may not change the value of a name the template already defines.<br />  - Overrides: claims may add new environment variables and override the values of names<br />    the template already defines.<br />Note: environment variables are baked into the Pod before it is created; they cannot be<br />injected into an already-running warm pool Pod. Consequently, allowing injection here<br />(Allowed or Overrides) only takes effect on a per-claim basis: any claim that actually<br />sets spec.env is forced to cold-start a fresh Sandbox and cannot adopt a warm pool<br />Sandbox. Claims that set no environment variables still use the warm pool normally. | Disallowed | Enum: [Allowed Overrides Disallowed] <br />Optional: \{\} <br /> |
| `volumeClaimTemplatesPolicy` _[VolumeClaimTemplatesPolicy](#volumeclaimtemplatespolicy)_ | volumeClaimTemplatesPolicy allows a SandboxClaim to inject or override volume claim templates defined in the template.<br />If set to Disallowed, the SandboxClaim will be rejected if it specifies any volume claim templates. | Disallowed | Enum: [Disallowed Allowed Overrides] <br />Optional: \{\} <br /> |


#### SandboxWarmPool



SandboxWarmPool is the Schema for the sandboxwarmpools API.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `extensions.agents.x-k8s.io/v1beta1` | | |
| `kind` _string_ | `SandboxWarmPool` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  | Optional: \{\} <br /> |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  | Optional: \{\} <br /> |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  | Optional: \{\} <br /> |
| `spec` _[SandboxWarmPoolSpec](#sandboxwarmpoolspec)_ | spec defines the desired state of SandboxWarmPool |  | Required: \{\} <br /> |
| `status` _[SandboxWarmPoolStatus](#sandboxwarmpoolstatus)_ | status defines the observed state of SandboxWarmPool |  | Optional: \{\} <br /> |


#### SandboxWarmPoolRef



SandboxWarmPoolRef references a SandboxWarmPool.



_Appears in:_
- [SandboxClaimSpec](#sandboxclaimspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the SandboxWarmPool |  | Required: \{\} <br /> |


#### SandboxWarmPoolRuntimeAdoption



SandboxWarmPoolRuntimeAdoption selects the opt-in runtime transfer contract.



_Appears in:_
- [SandboxWarmPoolSpec](#sandboxwarmpoolspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[SandboxWarmPoolRuntimeAdoptionMode](#sandboxwarmpoolruntimeadoptionmode)_ | mode requires identical immutable policy revisions and unused pool initialization.<br />Unsupported modes or runtime capabilities must never fall back to ungated warm adoption. |  | Enum: [SamePolicyFirstClaim] <br />Required: \{\} <br /> |


#### SandboxWarmPoolRuntimeAdoptionMode

_Underlying type:_ _string_

SandboxWarmPoolRuntimeAdoptionMode identifies an authenticated warm transfer protocol.

_Validation:_
- Enum: [SamePolicyFirstClaim]

_Appears in:_
- [SandboxWarmPoolRuntimeAdoption](#sandboxwarmpoolruntimeadoption)

| Field | Description |
| --- | --- |
| `SamePolicyFirstClaim` | SandboxWarmPoolRuntimeAdoptionSamePolicyFirstClaim permits only the first claim of<br />a verified unused pool activation under its exact existing policy revision set.<br /> |


#### SandboxWarmPoolSpec



SandboxWarmPoolSpec defines the desired state of SandboxWarmPool.



_Appears in:_
- [SandboxWarmPool](#sandboxwarmpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `runtimeAdoption` _[SandboxWarmPoolRuntimeAdoption](#sandboxwarmpoolruntimeadoption)_ | runtimeAdoption opts this pool into an authenticated runtime transfer protocol.<br />Omission preserves ordinary warm-pool behavior. A configured protocol remains<br />unavailable unless the complete controller and runtime handshake is supported. |  | Optional: \{\} <br /> |
| `replicas` _integer_ | replicas is the desired number of sandboxes in the pool.<br />This field is controlled by an HPA if specified. | 1 | Minimum: 0 <br />Optional: \{\} <br /> |
| `sandboxTemplateRef` _[SandboxTemplateRef](#sandboxtemplateref)_ | sandboxTemplateRef - name of the SandboxTemplate to be used for creating a Sandbox<br />Warning: Any change to the json tag "sandboxTemplateRef" must be synchronized with the TemplateRefField constant. |  | Required: \{\} <br /> |
| `updateStrategy` _[SandboxWarmPoolUpdateStrategy](#sandboxwarmpoolupdatestrategy)_ | updateStrategy controls how the pool replaces its stale sandboxes. A sandbox is<br />considered stale when the effective SandboxBlueprint derived from the referenced<br />SandboxTemplate (or the sandboxTemplateRef name) changes; metadata-only edits<br />(annotations or labels) do not make a sandbox stale and never trigger replacement.<br />It applies only to sandboxes still owned by the pool (i.e. unclaimed). Once a sandbox<br />is claimed by a SandboxClaim, ownership transfers to the claim and the pool no longer<br />manages or replaces it.<br />Defaults to OnReplenish. |  | Optional: \{\} <br /> |


#### SandboxWarmPoolStatus



SandboxWarmPoolStatus defines the observed state of SandboxWarmPool.



_Appears in:_
- [SandboxWarmPool](#sandboxwarmpool)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `replicas` _integer_ | replicas is the total number of sandboxes in the pool. |  | Optional: \{\} <br /> |
| `readyReplicas` _integer_ | readyReplicas is the total number of sandboxes in the pool that are in a ready state. |  | Optional: \{\} <br /> |
| `selector` _string_ | selector is the label selector used to find the pods in the pool. |  | Optional: \{\} <br /> |
| `observedGeneration` _integer_ | observedGeneration is the most recent generation observed by the controller.<br />It corresponds to the SandboxWarmPool's metadata.generation, which is bumped<br />on spec mutations such as replicas changes. Note that SandboxTemplate content<br />changes do not bump the pool's generation, so this does not track template<br />rollout progress. |  | Minimum: 0 <br />Optional: \{\} <br /> |


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
| `Recreate` | RecreateSandboxWarmPoolUpdateStrategyType deletes stale unclaimed sandboxes immediately<br />so the pool only holds fresh sandboxes matching the current template. Already-claimed<br />sandboxes are never touched.<br />Note: This applies to changes in the template's SandboxBlueprint only. Changes to annotations, labels, or template-level policies do not trigger recreate.<br /> |
| `OnReplenish` | OnReplenishSandboxWarmPoolUpdateStrategyType leaves stale unclaimed sandboxes in place.<br />A stale sandbox is only replaced with a fresh one when it is manually deleted, or when it<br />is claimed by a SandboxClaim (which removes it from the pool and triggers replenishment).<br />Already-claimed sandboxes are never touched.<br /> |


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


#### VolumeClaimTemplatesPolicy

_Underlying type:_ _string_

VolumeClaimTemplatesPolicy defines whether a SandboxClaim is allowed to inject or override volume claim templates.

_Validation:_
- Enum: [Disallowed Allowed Overrides]

_Appears in:_
- [SandboxTemplateSpec](#sandboxtemplatespec)

| Field | Description |
| --- | --- |
| `Disallowed` | VolumeClaimTemplatesPolicyDisallowed prevents a SandboxClaim from specifying any volume claim templates.<br /> |
| `Allowed` | VolumeClaimTemplatesPolicyAllowed allows a SandboxClaim to inject new volume claim templates, but not override existing ones.<br /> |
| `Overrides` | VolumeClaimTemplatesPolicyOverrides allows a SandboxClaim to inject new and override existing volume claim templates.<br /> |


