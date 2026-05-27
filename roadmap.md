## Roadmap

High-level overview of our main strategic priorities for 2026. This roadmap is categorized by key themes and highlights both completed and upcoming initiatives for the Open-Source (OSS) Kubernetes Agent Sandbox.

---

### 🚀 Core Functionality & Architecture

Core platform capabilities, controllers, scheduling engines, and backend interfaces.

*   **Decouple API from Runtime (Portable Backend)** (Owner: Lucky Abolorunke) `⏳ In Progress`
    *   Enable full customization of the runtime environment without breaking the API (via a common proto backend). [[KEP #597](https://github.com/kubernetes-sigs/agent-sandbox/pull/597), [KEP #747](https://github.com/kubernetes-sigs/agent-sandbox/pull/747)]
*   **SandboxTemplate & SandboxWarmPool Rolling Updates** (Owner: Laura Galbraith, Tomer Glottmann) `⏳ In Progress`
    *   Support rolling updates on WarmPools and Templates to update sandbox pods without causing downtime or service disruption. [[#323](https://github.com/kubernetes-sigs/agent-sandbox/issues/323)]
*   **1st Class Router** (Owner: Sairaj Pokale) `📅 Planned`
    *   Support the sandbox-router as a first-class citizen within the project (written in Go, built with the rest of the project, and shipping with out-of-the-box images).
*   **Auto Suspend/Resume** (Owner: Janet Kuo) `📅 Planned`
    *   Automatically suspend inactive sandboxes and resume them upon traffic or API invocation.
*   **Smart Warmpool Selection** (Owner: Vicente Ferrara Brondo) `⏳ In Progress`
    *   Support intelligent warmpool matching and routing based on claim requirements. [[#491](https://github.com/kubernetes-sigs/agent-sandbox/issues/491)]
*   **API Support for Multi-Sandbox per Pod** (Owner: Aditya Shantanu) `📅 Planned`
    *   Extend API models to support running and managing multiple isolated sandboxes inside a single Pod.

---

### 📦 SDKs & Client Libraries

Developer interfaces, programming language SDKs, and application-level tooling.

*   **Expand Python SDK Functionality** (Owner: Shruti Nair) `⏳ In Progress`
    *   Natively support high-level convenience methods such as reading/writing files, executing commands (`run_code`), and interactive tools.
*   **Typescript SDK Support** (Owner: Community) `⏳ In Progress`
    *   Implement high-level TypeScript SDK support for modern web application frontends.
*   **Client Interface for SDK Language Alignment** (Owner: Shruti Nair) `📅 Planned`
    *   Establish robust mechanism/interfaces to minimize language diversion across Python, Go, and TypeScript SDKs.
*   **Agent Sandbox MCP (Model Context Protocol) Server** (Owner: Tomer Glottmann) `📅 Planned`
    *   Integrate an MCP server endpoint via the router or SDK, making Agent Sandbox a native tool for MCP-enabled LLM runtimes.

---

### ⚡ Scale, Performance & Resource Optimization (Price-Perf)

Optimizing the operational footprint, reducing latencies, and lowering cloud/infrastructure costs.

*   **Extended Benchmarking & Better Performance** (Owner: Ivy Gooch) `📅 Planned`
    *   Benchmark large-scale workloads to identify performance bottlenecks, publish guidelines, and optimize controller throughput. *Improve the controller to handle 1000+ claims per second.*
*   **Improve Claim Latency (200ms ➔ 100ms ➔ 50ms)** (Owner: Ivy Gooch, Krzysztof Siedlecki) `📅 Planned`
    *   Analyze critical paths in the controller to reduce end-to-end sandbox assignment latencies down to sub-100ms.
*   **Scale to Zero** (Owner: Shrutiya Mohan, Tomer Glottmann) `📅 Planned`
    *   Scale sandbox replicas down to zero when inactive, preserving underlying resources while maintaining rapid resume paths.
*   **Measure & Improve TFFI (Time to First Instruction) Latency** (Owner: Aditya Shantanu) `📅 Planned`
    *   Define benchmarks and optimize the time required from invoking a sandbox to successfully executing the first code instruction.
*   **Support OpenClaw Price-Performance Targets** (Owner: Tomer Glottmann, Tinsley Shi) `⏳ In Progress`
    *   Optimize base-image size, runtime overhead, and cold start times to support microVM environments targeting extremely low cost limits.

---

### 🌐 Networking, Storage & Tenancy

Advanced ingress/egress isolation, lifecycle state retention, and security controls.

*   **Network Policy "Attach" at Claim Time** (Owner: Janet Kuo) `📅 Planned`
    *   Dynamic attachment of L4 and L7 egress/ingress NetworkPolicies at claim time to restrict internet access or whitelist specific FQDNs.
*   **Storage Customization at Claim Time** (Owner: Barni Seetharaman, Shrutiya Mohan) `📅 Planned`
    *   Allow full custom storage volume and PVC sizing definitions directly in the SandboxClaim. [[#225](https://github.com/kubernetes-sigs/agent-sandbox/issues/225), [#554](https://github.com/kubernetes-sigs/agent-sandbox/issues/554)]
*   **Strict Sandbox-to-Pod Mapping** (Owner: TBD) `⏳ In Progress`
    *   Provide bulletproof, deterministic 1-to-1 mappings between a Sandbox claim and its backing Pod. [[#127](https://github.com/kubernetes-sigs/agent-sandbox/issues/127)]
*   **Startup Actions** (Owner: Aditya Shantanu) `📅 Planned`
    *   Provide options for declarative startup routines, such as immediately pausing or scheduling suspension post-creation. [[#58](https://github.com/kubernetes-sigs/agent-sandbox/issues/58)]
*   **Auto-Deletion of Bursty Sandboxes** (Owner: Tomer Glottmann) `📅 Planned`
    *   Support automatic time-based or inactivity-based cleanup (TTL) for highly dynamic workloads like RL training.
*   **Detailed Logs Falco Configuration Extension** (Owner: TBD) `📅 Planned`
    *   Propagate deep-level container security configurations (e.g., Falco) to enable robust gVisor auditing.

---

### 🛡️ Observability & Quality of Life (KTLO)

Audit trails, custom telemetry, reliability, and automated regression testing.

*   **Alpha to Beta API Versioning** (Owner: Shruti Nair) `⏳ In Progress`
    *   Evolve the existing API schemas from alpha status toward robust beta APIs with deprecation safety.
*   **Security Fixes** (Owner: Chenyi Wang) `⏳ In Progress`
    *   Maintain active patching cycles for third-party dependencies and container base image security.
*   **CI for PodSnapshot & AgentSandbox Regression Prevention** (Owner: Shruti Nair) `⏳ In Progress`
    *   Introduce robust, isolated continuous integration tests to prevent regression.
*   **Controller Custom Metrics** (Owner: Ivy Gooch, Lucky Abolorunke) `⏳ In Progress`
    *   Track and expose standard metrics like sandbox creation latencies inside the controller. [[#125](https://github.com/kubernetes-sigs/agent-sandbox/pull/125)]
*   **Additional Prometheus Telemetry** (Owner: Ivy Gooch, Shruti Nair) `📅 Planned`
    *   Expose granular Prometheus counters to monitor API call frequencies, SDK usage, and overall controller performance.

---

### 🔌 Integrations & Ecosystem

Plugging into the broader AI Agent, reinforcement learning, and LLM framework ecosystem.

*   **Integration with Ray (Rllib)** (Owner: Vicente Ferrara Brondo) `⏳ In Progress`
    *   Seamless, high-performance container sandboxing for Ray training tasks.
*   **Integration with Agentic Frameworks** (Owner: Chenyi Wang) `⏳ In Progress`
    *   Provide native runtime execution environment plugins for LangChain, CrewAI, [OpenEnv](https://github.com/kubernetes-sigs/agent-sandbox/issues/132), kAgent and other tool-calling systems.
*   **Expand Sandbox Use Cases** (Owner: Shruti Nair) `📅 Planned`
    *   Add curated base images and setups tailored for interactive browser use-cases, computer-use actions, and terminal shells.
---

### 📝 Documentation, UI & Community Enablement

Lowering the barrier to entry, beautiful guides, interactive tools, and UI dashboards.

*   **UI Support in OSS** (Owner: Rajitha Leonhard, Ivy Gooch) `📅 Planned`
    *   Build a lightweight open-source web dashboard/UI to visually inspect active sandboxes, warmpools, and templates.
*   **Publish Benchmarking Methodology & Guides** (Owner: Ivy Gooch) `⏳ In Progress`
    *   Share systematic methodologies, configs, and reference results of running large-scale workloads.
*   **Reference Architectures** (Owner: Tomer Glottmann) `📅 Planned`
    *   Document production-ready reference designs for multi-user cloud environments.


## Completed (Since v0.0.1)
*   **Golang SDK Support** (Owner: Community) `✅ Completed`
    *   Deliver high-level Go client libraries to programmatically manage sandboxes and route connections. [[#227](https://github.com/kubernetes-sigs/agent-sandbox/issues/227)]
*   **PyPI Distribution (`k8s-agent-sandbox`)** (Owner: Shrutiya Mohan) `✅ Completed`
    *   Publish the client library to PyPI for seamless installation and usage. [[#146](https://github.com/kubernetes-sigs/agent-sandbox/issues/146)]
*   **Runtime API OTEL/Tracing Instrumentation** (Owner: Ivy Gooch) `✅ Completed`
    *   Fully instrument the sandbox runtime APIs using OpenTelemetry/Tracing to aid debugging.
*   **Metadata Propagation** (Owner: Chenyi Wang) `✅ Completed`
    *   Ensure proper transmission of claim-level labels and annotations to underlying sandbox pods. [[#173](https://github.com/kubernetes-sigs/agent-sandbox/pull/173)]
*   **Status Updates** (Owner: Shruti Nair) `✅ Completed`
    *   Properly reflect actual sandbox lifecycle phases (Pending, Ready, Suspended, etc.) within status structures. [[#121](https://github.com/kubernetes-sigs/agent-sandbox/pull/121)]
*   **Integration with HPA & Cold Standby Nodes (CSN)** (Owner: Shrutiya Mohan, Tomer Glottmann) `✅ Completed`
    *   Optimize the combination of warmpools with horizontal pod autoscaling and cold standby nodes to drastically reduce idle infrastructure costs.
*   **Controller Optimization for High-Throughput Claims** `✅ Completed`
    *   Optimize the controller to handle extreme claim burst throughput (up to 300 sandboxes/second) without resource degradation.
*   **Suspend / Resume (PVC-based)** (Owner: Justin Santa Barbara, Tomer Glottmann) `✅ Completed`
    *   Enable full state suspension preserving PVC storage: when scaled to 0, the PVC is persisted and cleanly attached back when resumed.
*   **Headless Service Port Handling** (Owner: TBD) `✅ Completed`
    *   Ensure headless services map configured containerPorts accurately for multi-port routing. [[#156](https://github.com/kubernetes-sigs/agent-sandbox/pull/156)]
*   **Overhaul Documentation** (Owner: Joan Kallogjeri) `✅ Completed`
    *   Restructure and write comprehensive, high-fidelity guides replicating clear, professional developer-oriented styles.
*   **Website Refresh** (Owner: TBD) `✅ Completed`
    *   Ensure that the public site reflects current API changes, usage examples, and best-practice architectures. [[#166](https://github.com/kubernetes-sigs/agent-sandbox/issues/166)]