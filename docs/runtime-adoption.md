# Same-policy runtime adoption

Agent Sandbox can coordinate the first claim of an unused Gatekeeper Runtime
strict pool execution through a protected reservation. The contract preserves
the original Pod, container task, and managed root. Gatekeeper Runtime must hold
that execution, verify the final claim metadata and current policy, and confirm
release under the successor activation before the claim becomes ready.

This integration is disabled by default. The paired runtime must pass its native
hold and ownership-transfer conformance checks before advertising
`atomicSamePolicyTransfer`. Installing the webhook or opting in a pool does not
qualify a runtime. The current implementation makes no native conformance claim.

## Platform configuration

The Helm chart serves admission only when both `controller.extensions` and
`runtimeAdoption.enabled` are true. Supply a TLS Secret containing `tls.crt` and
`tls.key`, with a certificate valid for
`agent-sandbox-runtime-adoption.<controller-namespace>.svc`, and the base64 PEM CA
bundle that signed it. For example, a platform's values file can contain:

```yaml
controller:
  extensions: true
runtimeAdoption:
  enabled: true
  tlsSecretName: runtime-adoption-tls
  caBundle: <base64-encoded-PEM-CA>
  runtimeControllerNamespace: gatekeeper-runtime-system
  agentNamespace: gatekeeper-runtime-agent-system
```

The chart creates the HTTPS Service and an unfiltered, fail-closed validating
webhook. It protects Sandbox and SandboxClaim status, Pod metadata, Pod status,
ephemeral containers, resize, deletion, eviction, exec, attach, port forwarding,
and RuntimePolicyNodeStatus writes. Mutable workload labels must not select
which objects receive these checks. Platform administrators control the webhook,
its certificate, and the service-account identities.

For the first installation, or when first enabling admission, render the enabled
chart and apply its CRDs, TLS Secret, and controller resources before applying
the `ValidatingWebhookConfiguration`. Wait for the controller Deployment to be
ready, then apply the registration. The fail-closed Pod webhook can otherwise
block creation of its own backend. New adoption remains unavailable while that
registration is absent. Subsequent upgrades must keep the protection registered
while a serving controller remains available.

The companion controller owns `status.runtimeAdoption` and the
`agents.x-k8s.io/runtime-adoption-protection` finalizer. The separate GKR readiness
controller owns `status.runtimeActivationVerification` and the Pod's
`runtime.gatekeeper.sh/ActivationReady` condition. A runtime agent can publish
node evidence only with credentials bound to its live Pod and Node UID. The
default agent namespace is `gatekeeper-runtime-agent-system`, distinct from the
GKR controller namespace. Configure namespace overrides consistently in both
projects.

For non-Helm packaging, the manager's platform flags start with
`--runtime-adoption-admission=true` and require `--extensions`. The serving port
is 9443 and the certificate directory is `/etc/runtime-adoption/tls`. The plain
installation manifests do not install this optional webhook or its certificate.

## Pool and claim behavior

A pool requests the contract declaratively:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: strict-agents
spec:
  sandboxTemplateRef:
    name: strict-agent
  replicas: 2
  runtimeAdoption:
    mode: SamePolicyFirstClaim
```

Its template must use `gatekeeper-runtime-strict` and the paired runtime's
eligible blueprint. The contract requires a digest-pinned application image,
one application container, no init or ephemeral containers, and no mutable or
shared external workload state. Configuration references must satisfy GKR's
immutable object and UID pinning checks. Claims that add environment variables
or volume claim templates use cold activation.

Without qualified, fresh source evidence, a new claim falls back to cold
activation. Labels only locate candidates. The companion verifies the live
pool, template, namespace, Sandbox, Pod and Node UIDs against the runtime's
signed cold initialization before reserving a candidate.

The companion first persists the claim attempt, then acquires the Sandbox with
a resource-version check. It retains a consumed marker permanently. Once the
node proves that the task is held, the companion changes the Sandbox owner and
target metadata. The core controller writes the corresponding Pod metadata
synchronously and reads it back before the companion acknowledges its digest.

Release authorization has two durable steps in both objects. First the
companion records the held receipt and context. After GKR signs the start grant,
the companion records that grant's digest. A partial write cannot authorize
release. Retries finish the same attempt. A confirmed commit still requires
current GKR verification for the exact claim, Pod, container task, Node and
successor activation before the claim becomes ready.

Pending reservations continue to consume pool capacity after their pool label
and owner reference change. They cannot be garbage-collected or handed to a
second claim. Replenishment resumes after confirmed commit or original-root
destruction.

## Cancellation and recovery

Claim changes, expiry, deletion, missing execution, and lost qualification before
commit request termination. A timeout or missing API object does not prove that
a runtime effect did not happen. The attempt and its protection finalizer remain
until a fresh authenticated node observation confirms the outcome.

Destruction evidence allows the companion to finalize the consumed Sandbox.
A durable rejection of an unacquired attempt allows only that claim to finish
and may clear an empty preparation finalizer. It never allows deletion of a
Sandbox reserved by another claim. Node journals must retain these outcomes
until the protected API records acknowledge them. Do not clear an uncertain
attempt's status or remove its finalizer manually.

## Compatibility

Legacy warm adoption and its annotation, label, conflict-recovery and
`AlreadyExists` paths refuse strict pool executions. This changes prior strict
warm-pool behavior, including deployments where the new opt-in is disabled.
Ordinary runtimes keep their existing adoption path. Cold strict activation
continues to work.

Treat this as a coordinated companion and GKR upgrade. Drain and replace legacy
strict pool members and existing claims through their normal cold lifecycle
before using the new contract. Existing executions cannot be retroactively
enrolled as fresh pool initializations. Do not enable runtime qualification until
the matching native profile passes conformance.

## Verification

The controller tests pass real patches through the admission handler and cover
claim-first reservation, held metadata ordering, interrupted two-object grants,
controller restart, current-claim readiness, losing claims, and recovery after
Sandbox deletion. Other tests exercise pool deletion races, capacity accounting,
protected status preservation and bound node-evidence writers.

`internal/runtimeadoption/testdata` contains shared canonical wire, claim-intent,
metadata and secure-default fixtures. Both projects must consume the same bytes.
These API and controller tests do not establish kernel hold or release behavior;
that requires the paired runtime's native conformance suite.
