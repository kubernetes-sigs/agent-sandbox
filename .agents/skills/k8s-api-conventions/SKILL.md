---
name: k8s-api-conventions
description: Guides the agent to follow Kubernetes API conventions for OSS standards.
---

# Kubernetes API Conventions Skill

## Purpose
This skill ensures that all Custom Resource Definitions (CRDs) generated or modified in this project follow the established conventions defined by the Kubernetes community.

## Instructions
1.  **CRDs as First-Class APIs**: Adhere to the guidelines in the official Kubernetes API conventions. CRDs must follow the same conventions regarding field naming, types, and structure (Spec/Status separation) as core Kubernetes resources.
2.  **Review References**: Before creating or modifying any CRD definitions in this project, review the linked community guidelines.
3.  **Commonly Missed Conventions ("Gotchas")**: Pay special attention to the following design points derived from internal experience, which automated linters often miss:
    *   **Label Values**: Do NOT put full resource names in label values. Resource names can exceed the label value size limit (63 characters).
    *   **Preview Features**: Do NOT use annotations for alpha/preview features. Use new API fields instead, to avoid migration difficulties later.
    *   **Status Properties**: Use `conditions` instead of `phase` for tracking state.
    *   **Mutating Spec**: The `spec` of the primary Custom Resource (CR) being reconciled is user-owned and should not be modified and saved back to the API server by the reconciler. This avoids mutating user intent. Controllers may, however, create and update the `spec` of **secondary or target** objects (for example, the HPA controller updating a Deployment's `spec.replicas`).
    *   **Zero vs. Unset**: Use pointers for fields where it is important to distinguish between a zero value (e.g., `0`) and the field being unset.
    *   **Scalability**: Avoid storing unbounded lists of items in the API (etcd has size limits). Consider aggregating or summarizing lists in `status`.
    *   **Think twice about booleans**: Avoid booleans for fields that might evolve to have more states in the future. Use enums or string fields instead.
    *   **Declarative Field Names**: Ensure field names describe the desired state, not an action (e.g., use `suspended` instead of `suspend`).

## References
- [Kubernetes API Conventions](references/api-conventions.md)
