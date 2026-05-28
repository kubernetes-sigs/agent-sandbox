# Security Threat Model

This document outlines the security threat model for `agent-sandbox`. It defines the trust boundaries, identifies key threat scenarios, and documents the mitigations designed to ensure strong tenant isolation when executing unpredictable or untrusted AI-generated workloads.

---

## 1. Trust Boundaries

In an `agent-sandbox` deployment, resources are divided across three distinct trust levels:

```mermaid
graph TD
    subgraph Cluster Control Plane (Highly Trusted)
        A[Cluster Admin] --> B[Controller Manager]
    end

    subgraph Namespace / Tenant Plane (Untrusted)
        B --> C[Sandbox CRD]
        C --> D[Sandbox Pod (gVisor / Kata)]
        C --> E[Headless Service]
        C --> F[Tenant PVC]
    end

    classDef trusted fill:#e1f5fe,stroke:#0288d1,stroke-width:2px;
    classDef untrusted fill:#ffebee,stroke:#c62828,stroke-width:2px;
    class A,B trusted;
    class C,D,E,F untrusted;
```

*   **Cluster Control Plane (Highly Trusted)**: Encompasses cluster administrators and the controller manager. The controller operates with cluster-wide or namespace-restricted permissions necessary to manage Pods, Services, and PVCs.
*   **Tenant Context (Untrusted)**: Workloads executing inside individual Sandbox pods (e.g., LLM-generated code, terminal tasks). These workloads are completely untrusted and must be strongly isolated from both the host and other sandboxes.

---

## 2. Threat Scenarios and Mitigations

### Threat A: Pod Selector Hijacking via Metadata Spoofing (Headless Service Hijack)
*   **Description**: A malicious tenant could attempt to set system-reserved metadata—specifically the `agents.x-k8s.io/sandbox-name-hash` label—inside their own Sandbox's `Spec.PodTemplate.ObjectMeta.Labels` configuration. If successfully propagated to their Pod, the Headless Service selector (which routes traffic to sandboxes by name-hash) would route traffic intended for a victim tenant to the attacker's pod.
*   **Severity**: **High** (Loss of isolation, data interception).
*   **Mitigation**: 
    *   **Domain-Wide Metadata Filtering**: The sandbox controller explicitly ignores any user-provided labels or annotations starting with the `agents.x-k8s.io/` or `extensions.agents.x-k8s.io/` domain-wide prefixes inside the PodTemplate.
    *   **Precise System Cleanup**: During reconciliation, the controller compares the Pod's labels and annotations against the user-provided `Spec.PodTemplate`. If it detects a system-reserved label or annotation that was attempted to be injected via the PodTemplate, it removes it immediately. Unrelated system metadata added by external platform controllers (e.g., service mesh proxies or custom CNIs) is preserved safely.

---

### Threat B: Privilege Escalation via ServiceAccount Spoofing
*   **Description**: A tenant sandbox could attempt to gain elevated permissions by associating itself with a highly-privileged ServiceAccount (e.g., `cluster-admin` or one that can edit secrets/deployments).
*   **Severity**: **High** (Cluster compromise).
*   **Mitigation**:
    *   **Validating Admission Policies (VAP) & OPA Gatekeeper**: Administrators must deploy VAP or OPA policies to restrict Sandbox pods to a pre-approved list of low-privilege ServiceAccounts.
    *   **ServiceAccount Token Disabling**: Whenever possible, templates should default to `automountServiceAccountToken: false` to prevent arbitrary API server calls from inside the sandbox.

---

### Threat C: Container Escape and Host Compromise
*   **Description**: A malicious process inside a sandbox container attempts a kernel exploit to escape the container namespaces and gain root access to the underlying Kubernetes worker node.
*   **Severity**: **Critical** (Host node compromise).
*   **Mitigation**:
    *   **Virtualization & Sandboxing Runtimes**: The threat model assumes that sandboxes are executed under micro-virtualized runtime environments like **gVisor (`runsc`)** or **Kata Containers** to provide strong kernel-level isolation.
    *   **Restricted Pod Security Standards (PSS)**: By default, admission policies (like GKE Validating Admission Policy or Kyverno) enforce `privileged: false`, `runAsNonRoot: true`, and drop `ALL` linux capabilities on all sandbox Pods.

---

## 3. Shared Responsibility Model

Securing `agent-sandbox` requires a shared effort between the core controller manager and the cluster platform team:

| Layer | Responsible Party | Security Focus |
|---|---|---|
| **Application Spec** | Developer / Client SDK | Opt-in to secure templates, enforce low-privilege environments, disable ServiceAccount tokens. |
| **Metadata Integrity** | Sandbox Controller | Block label/annotation injection attacks, manage cryptographic name-hash generation, adoption validation. |
| **Kernel/Host Isolation** | Cluster Admin | Enforce gVisor/Kata runtime classes, restrict container execution capabilities (`runAsNonRoot`). |
| **Network Boundary** | Cluster Admin / CNI | Use Calico/Cilium NetworkPolicies to enforce namespace and pod isolation boundaries. |
