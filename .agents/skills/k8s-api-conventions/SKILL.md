---
name: k8s-api-conventions
description: Guides the agent to follow Kubernetes API conventions for OSS standards.
---

# Kubernetes API Conventions Skill

## Purpose
This skill ensures that all Custom Resource Definitions (CRDs) generated or modified in this project follow the established conventions defined by the Kubernetes community.

## Instructions
1.  **CRDs as First-Class APIs**: Adhere to the guidelines in the official Kubernetes API conventions. CRDs must follow the same conventions regarding field naming, types, and structure (Spec/Status separation) as core Kubernetes resources.
2.  **Primary Guidelines**: Rely on the condensed "Gotchas" below for 90% of standard CRD and API reviews. These represent the most common compliance failures derived from Kubernetes API conventions and community best practices, which automated linters often miss:
    *   **Label Values**: Do NOT use full resource names as label values. Kubernetes enforces a strict 63-character limit on label values, whereas resource names can be up to 253 characters. Using full resource names in labels can lead to asynchronous runtime failures (e.g., a parent resource is successfully created, but the controller continuously fails to create child resources due to label length limits). When resource names must be reflected in labels, implement safe truncation or hashing in the controller logic to ensure values remain under 63 characters.
    *   **Preview Features**: Do NOT use annotations for alpha/preview features. Use new API fields instead, to avoid migration difficulties later.
    *   **Status Properties**: Use `conditions` instead of `phase` for tracking state.
    *   **Mutating Spec**: The `spec` of the primary Custom Resource (CR) being reconciled is user-owned and should not be modified and saved back to the API server by the reconciler. This avoids mutating user intent. Controllers may, however, create and update the `spec` of **secondary or target** objects (for example, the HPA controller updating a Deployment's `spec.replicas`).
    *   **Zero vs. Unset**: Use pointers for fields where it is important to distinguish between a zero value (e.g., `0`) and the field being unset.
    *   **Scalability**: Avoid storing unbounded lists of items in the API (etcd has size limits). Consider aggregating or summarizing lists in `status`.
    *   **Metrics Cardinality & Normalization**: Never introduce high or unbounded cardinality Prometheus labels (e.g., pod names, UIDs, timestamps, raw errors). When deriving label values from dynamic input or errors, apply metrics normalization (an allowlist switch or categorizer) to map strings into a small, fixed enum.
    *   **Think twice about booleans**: Avoid booleans for fields that might evolve to have more states in the future. Use enums or string fields instead.
    *   **Declarative Field Names**: Ensure field names describe the desired state, not an action (e.g., use `suspended` instead of `suspend`).
    *   **Lists over Maps**: Do not use maps of subobjects (e.g., `ports: {www: {port: 80}}`). Use a list of subobjects containing a `name` field (e.g., `ports: [{name: www, port: 80}]`). The only exceptions are pure string maps (labels, annotations).
    *   **Integer & Float Types**: Always use explicit `int32` or `int64` (preferring `int32`), never ambiguously sized `int` or unsigned integers (`uint`). Avoid floating-point types entirely in `spec`.
    *   **Duration & Timestamp Naming**: Express durations with a `Seconds` suffix (e.g., `periodSeconds`, `timeoutSeconds`). For timestamps, use `somethingTime` (e.g., `lastTransitionTime`), avoiding the word `stamp`.
    *   **Allocated Values in Status**: If a controller automatically allocates a resource (like a ClusterIP, port number, or storage ID) on behalf of the user, store the resulting allocated value in `status`, not `spec`.
    *   **Breaking Changes**: Whenever reviewing or proposing changes that alter existing default runtime behaviors, flag them as breaking changes that require deprecation cycles and migration paths.
    *   **Controller CLI Flags vs. Declarative CRs**: Do NOT use global controller command-line flags to configure tenant workload semantics or operational behaviors; all workload behavior must be configured declaratively via Custom Resources. When global CLI flags exist for platform-level baseline governance, ensure declarative Custom Resource configuration always takes precedence.
    *   **API Schema Minimization**: Do NOT expand CRD schemas with new fields unless strictly necessary. Avoid toggle proliferation (introducing multiple overlapping configuration mechanisms for a single behavior) and establish clear, declarative precedence hierarchies.
    *   **Controller Logging Discipline**: In high-frequency Reconcile loops, reserve `logger.Info` / `V(0)` for major state changes and lifecycle milestones (e.g., resource created, claim adopted). Require `logger.V(4).Info` for routine steady-state checks, cache lookups, or status fall-throughs.
    *   **Call-Site & Downstream Impact Auditing**: When modifying helper predicates, validation functions, or error return criteria, audit all call sites across reconcilers to prevent downstream side effects (e.g., premature queue evictions or cache drops).
3.  **Deep-Dive Reference**: If you encounter complex architectural ambiguity, custom subresources, or edge cases not covered by the Gotchas above, consult the full upstream specification at `references/api-conventions.md`.

## References
- [Kubernetes API Conventions](references/api-conventions.md)
