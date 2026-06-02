## Roadmap

High-level overview of our main strategic priorities for 2026. This roadmap is categorized by key themes and highlights both completed and upcoming initiatives for the Open-Source (OSS) Kubernetes Agent Sandbox.

---

### 🚀 Core Functionality & Architecture

Core platform capabilities, controllers, scheduling engines, and backend interfaces.

*   **Decouple API from Runtime (Portable Backend)** `⏳ In Progress`
    *   Enable full customization of the runtime environment without breaking the API (via a common proto backend). [[KEP #597](https://github.com/kubernetes-sigs/agent-sandbox/pull/597), [KEP #747](https://github.com/kubernetes-sigs/agent-sandbox/pull/747)]
*   **SandboxTemplate & SandboxWarmPool Rolling Updates** `⏳ In Progress`
    *   Support rolling updates on WarmPools and Templates to update sandbox pods without causing downtime or service disruption. [[#323](https://github.com/kubernetes-sigs/agent-sandbox/issues/323)]
*   **1st Class Router** `📅 Planned`
    *   Support the sandbox-router as a first-class citizen within the project (written in Go, built with the rest of the project, and shipping with out-of-the-box images).
*   **Auto Suspend/Resume** `📅 Planned`
    *   Automatically suspend inactive sandboxes and resume them upon traffic or API invocation.
*   **Smart Warmpool Selection** `⏳ In Progress`
    *   Support intelligent warmpool matching and routing based on claim requirements. [[#491](https://github.com/kubernetes-sigs/agent-sandbox/issues/491)]
*   **API Support for Multi-Sandbox per Pod** `📅 Planned`
    *   Extend API models to support running and managing multiple isolated sandboxes inside a single Pod.

---

### 📦 SDKs & Client Libraries

Developer interfaces, programming language SDKs, and application-level tooling.

*   **Expand Python SDK Functionality** `⏳ In Progress`
    *   Natively support high-level convenience methods such as reading/writing files, executing commands (`run_code`), and interactive tools.
*   **Typescript SDK Support** `⏳ In Progress`
    *   Implement high-level TypeScript SDK support for modern web application frontends.
*   **Client Interface for SDK Language Alignment** `📅 Planned`
    *   Establish robust mechanism/interfaces to minimize language diversion across Python, Go, and TypeScript SDKs.
*   **Agent Sandbox MCP (Model Context Protocol) Server** `📅 Planned`
    *   Integrate an MCP server endpoint via the router or SDK, making Agent Sandbox a native tool for MCP-enabled LLM runtimes.

---

### ⚡ Scale, Performance & Resource Optimization (Price-Perf)

Optimizing the operational footprint, reducing latencies, and lowering cloud/infrastructure costs.

*   **Extended Benchmarking & Better Performance** `📅 Planned`
    *   Benchmark large-scale workloads to identify performance bottlenecks, publish guidelines, and optimize controller throughput. *Improve the controller to handle 1000+ claims per second.*
*   **Improve Claim Latency (200ms ➔ 100ms ➔ 50ms)** `📅 Planned`
    *   Analyze critical paths in the controller to reduce end-to-end sandbox assignment latencies down to sub-100ms.
*   **Scale to Zero** `📅 Planned`
    *   Suspend sandboxes when inactive, preserving underlying resources while maintaining rapid resume paths.
*   **Measure & Improve TFFI (Time to First Instruction) Latency** `📅 Planned`
    *   Define benchmarks and optimize the time required from invoking a sandbox to successfully executing the first code instruction.
*   **Support OpenClaw Price-Performance Targets** `⏳ In Progress`
    *   Optimize base-image size, runtime overhead, and cold start times to support microVM environments targeting extremely low cost limits.

---

### 🌐 Networking, Storage & Tenancy

Advanced ingress/egress isolation, lifecycle state retention, and security controls.

*   **Sandbox / Pod Identity Association** `📅 Planned`
    *   Enable dynamic sandbox/pod identity allocation at claim time (especially when provisioning from pre-warmed pools), associating the pod's security principal/identity based on the user/system making the SandboxClaim.
*   **Network Policy "Attach" at Claim Time** `📅 Planned`
    *   Dynamic attachment of L4 and L7 egress/ingress NetworkPolicies at claim time to restrict internet access or whitelist specific FQDNs.
*   **Storage Customization at Claim Time** `📅 Planned`
    *   Allow full custom storage volume and PVC sizing definitions directly in the SandboxClaim. [[#225](https://github.com/kubernetes-sigs/agent-sandbox/issues/225), [#554](https://github.com/kubernetes-sigs/agent-sandbox/issues/554)]
*   **Strict Sandbox-to-Pod Mapping** `⏳ In Progress`
    *   Provide bulletproof, deterministic 1-to-1 mappings between a Sandbox claim and its backing Pod. [[#127](https://github.com/kubernetes-sigs/agent-sandbox/issues/127)]
*   **Startup Actions** `📅 Planned`
    *   Provide options for declarative startup routines, such as immediately pausing or scheduling suspension post-creation. [[#58](https://github.com/kubernetes-sigs/agent-sandbox/issues/58)]
*   **Auto-Deletion of Bursty Sandboxes** `📅 Planned`
    *   Support automatic time-based or inactivity-based cleanup (TTL) for highly dynamic workloads like RL training.
*   **Detailed Logs Falco Configuration Extension** `📅 Planned`
    *   Propagate deep-level container security configurations (e.g., Falco) to enable robust gVisor auditing.

---

### 🛡️ Observability & Quality of Life (KTLO)

Audit trails, custom telemetry, reliability, and automated regression testing.

*   **Alpha to Beta API Versioning** `⏳ In Progress`
    *   Evolve the existing API schemas from alpha status toward robust beta APIs with deprecation safety.
*   **Security Fixes** `⏳ In Progress`
    *   Maintain active patching cycles for third-party dependencies and container base image security.
*   **CI for PodSnapshot & AgentSandbox Regression Prevention** `⏳ In Progress`
    *   Introduce robust, isolated continuous integration tests to prevent regression.
*   **Controller Custom Metrics** `⏳ In Progress`
    *   Track and expose standard metrics like sandbox creation latencies inside the controller. [[#125](https://github.com/kubernetes-sigs/agent-sandbox/pull/125)]
*   **Additional Prometheus Telemetry** `📅 Planned`
    *   Expose granular Prometheus counters to monitor API call frequencies, SDK usage, and overall controller performance.

---

### 🔌 Integrations & Ecosystem

Plugging into the broader AI Agent, reinforcement learning, and LLM framework ecosystem.

*   **Integration with Ray (Rllib)** `⏳ In Progress`
    *   Seamless, high-performance container sandboxing for Ray training tasks.
*   **Integration with Agentic Frameworks** `⏳ In Progress`
    *   Provide native runtime execution environment plugins for LangChain, CrewAI, [OpenEnv](https://github.com/kubernetes-sigs/agent-sandbox/issues/132), kAgent and other tool-calling systems.
*   **Expand Sandbox Use Cases** `📅 Planned`
    *   Add curated base images and setups tailored for interactive browser use-cases, computer-use actions, and terminal shells.

---

### 📝 Documentation, UI & Community Enablement

Lowering the barrier to entry, beautiful guides, interactive tools, and UI dashboards.

*   **UI Support in OSS** `📅 Planned`
    *   Build a lightweight open-source web dashboard/UI to visually inspect active sandboxes, warmpools, and templates.
*   **Publish Benchmarking Methodology & Guides** `⏳ In Progress`
    *   Share systematic methodologies, configs, and reference results of running large-scale workloads.
*   **Reference Architectures** `📅 Planned`
    *   Document production-ready reference designs for multi-user cloud environments.


## Completed (Since v0.0.1)
*   **Golang SDK Support** `✅ Completed`
    *   Deliver high-level Go client libraries to programmatically manage sandboxes and route connections. [[#227](https://github.com/kubernetes-sigs/agent-sandbox/issues/227)]
*   **PyPI Distribution (`k8s-agent-sandbox`)** `✅ Completed`
    *   Publish the client library to PyPI for seamless installation and usage. [[#146](https://github.com/kubernetes-sigs/agent-sandbox/issues/146)]
*   **Runtime API OTEL/Tracing Instrumentation** `✅ Completed`
    *   Fully instrument the sandbox runtime APIs using OpenTelemetry/Tracing to aid debugging.
*   **Metadata Propagation** `✅ Completed`
    *   Ensure proper transmission of claim-level labels and annotations to underlying sandbox pods. [[#173](https://github.com/kubernetes-sigs/agent-sandbox/pull/173)]
*   **Status Updates** `✅ Completed`
    *   Properly reflect actual sandbox lifecycle phases (Pending, Ready, Suspended, etc.) within status structures. [[#121](https://github.com/kubernetes-sigs/agent-sandbox/pull/121)]
*   **Integration with HPA & Cold Standby Nodes (CSN)** `✅ Completed`
    *   Optimize the combination of warmpools with horizontal pod autoscaling and cold standby nodes to drastically reduce idle infrastructure costs.
*   **Controller Optimization for High-Throughput Claims** `✅ Completed`
    *   Optimize the controller to handle extreme claim burst throughput (up to 300 sandboxes/second) without resource degradation.
*   **Suspend / Resume (PVC-based)** `✅ Completed`
    *   Enable full state suspension preserving PVC storage: when scaled to 0, the PVC is persisted and cleanly attached back when resumed.
*   **Headless Service Port Handling** `✅ Completed`
    *   Ensure headless services map configured containerPorts accurately for multi-port routing. [[#156](https://github.com/kubernetes-sigs/agent-sandbox/pull/156)]
*   **Overhaul Documentation** `✅ Completed`
    *   Restructure and write comprehensive, high-fidelity guides replicating clear, professional developer-oriented styles.
*   **Website Refresh** `✅ Completed`
    *   Ensure that the public site reflects current API changes, usage examples, and best-practice architectures. [[#166](https://github.com/kubernetes-sigs/agent-sandbox/issues/166)]
