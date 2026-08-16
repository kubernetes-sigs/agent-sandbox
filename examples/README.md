# Agent Sandbox Examples

This directory contains examples of how to use the Agent Sandbox. Each subdirectory contains a different example.

- [**agent-sandbox-rl**](./agent-sandbox-rl): Generic, multi-cluster batch orchestration for running SWE-bench-style RL/eval workloads on Agent Sandbox warm pools.
- [**aio-sandbox**](./aio-sandbox): An example of running All-in-One (AIO) Sandbox using agent-sandbox.
- [**apf-insulation**](./apf-insulation): An opt-in API Priority and Fairness overlay giving the controller dedicated apiserver concurrency for high-rate claim workloads (claim path > bulk refill > events).
- [**chrome-sandbox**](./chrome-sandbox): An example of running a Chrome browser in a sandbox.
- [**code-interpreter-agent-on-adk**](./code-interpreter-agent-on-adk): An example of using Agent Sandbox as a tool in Agent Development Kit (ADK).
- [**composing-sandbox-nw-policies**](./composing-sandbox-nw-policies): An example of composing network policies for sandboxes.
- [**containarium-ssh-sandbox**](./containarium-ssh-sandbox): An example of running Containarium's agent-box runtime in a Sandbox, reached over SSH with an in-container MCP server (no kube-apiserver token held by the agent).
- [**envd-sandbox**](./envd-sandbox): An example of running E2B's envd daemon as the container entrypoint, providing an E2B-compatible REST and gRPC API for filesystem, process execution, and metrics.
- [**firecracker-sandbox**](./firecracker-sandbox): An example of running a sandbox on Kata Containers with the Firecracker VMM (`kata-fc`) plus an envd-compatible runtime that matches the E2B data-plane contract.
- [**gemini-cu-sandbox**](./gemini-cu-sandbox): An example of a Python runtime sandbox for Gemini Computer Use Agent.
- [**gke-swap**](./gke-swap): Demonstrates how to configure GKE node memory swap with dedicated Local SSDs to drastically increase Chrome pod density from 120 to 200 pods per node.
- [**hello-world-sandbox**](./hello-world-sandbox): A simple "Hello World" sandbox example.
- [**hermes-agent**](./hermes-agent): An example of running Hermes Agent with persistence and custom skills.
- [**hermes-agents-as-a-service**](./hermes-agents-as-a-service): The multi-user platform pattern distilled from a real agents-as-a-service product: per-user claims over a warm pool, suspend/resume as the cost dial, PVC state survival, and injection-policy enforcement.
- [**hpa-swp-scaling**](./hpa-swp-scaling): An example of scaling a SandboxWarmPool using Kubernetes Horizontal Pod Autoscaler (HPA).
- [**jupyterlab**](./jupyterlab): An example of running JupyterLab on Agent-Sandbox.
- [**kata-aks**](./kata-aks): Full end-to-end example — Python agent, Go client, and sandbox-router — on AKS with Kata Containers (Hyper-V) VM isolation and the warm-pool/claim pattern.
- [**kata-aks-sandbox**](./kata-aks-sandbox): An example of running a sandbox with Kata Containers hardware-virtualized isolation on AKS, using the built-in Pod Sandboxing feature.
- [**kata-gke-sandbox**](./kata-gke-sandbox): An example of running a sandbox with Kata Containers hardware-virtualized isolation on GKE.
- [**keda-scale-to-zero**](./keda-scale-to-zero): An example of scaling a SandboxWarmPool down to zero (and back up) using KEDA and Google Managed Service for Prometheus (GMP).
- [**langchain**](./langchain): An example of a coding agent using Agent-Sandbox and LangGraph.
- [**manual-pdb**](./manual-pdb): An example of manual PodDisruptionBudget (PDB) configuration for sandboxes.
- [**mcp-server-sandbox**](./mcp-server-sandbox): Run an MCP (Model Context Protocol) server inside a Sandbox with attached storage.
- [**nono-sandbox**](./nono-sandbox): An example of running nono inside an Agent Sandbox, with fine-grained filesystem isolation, network filtering, credential brokering, and ephemeral per-tool micro-sandboxes.
- [**nullclaw-sandbox**](./nullclaw-sandbox): An example of running Nullclaw, a minimal AI assistant runtime, inside the Agent Sandbox.
- [**openclaw-gvisor-sandbox**](./openclaw-gvisor-sandbox): A production-shaped, gVisor-isolated OpenClaw sandbox using the template/claim pattern and persistent storage.
- [**openclaw-kata-aks-sandbox**](./openclaw-kata-aks-sandbox): An OpenClaw sandbox isolated by Kata Containers on AKS, so the agent runtime gets its own VM and guest kernel.
- [**playwright-sandbox**](./playwright-sandbox): An example of running Playwright with Chromium in a sandbox for web scraping and screenshots.
- [**policy**](./policy): Examples of using different policies with sandboxes.
- [**python-runtime-sandbox**](./python-runtime-sandbox): An example of a Python runtime sandbox.
- [**sandbox-ksa**](./sandbox-ksa): Examples of a sandbox with a service account, namespace, and a basic sandbox.
- [**vscode-sandbox**](./vscode-sandbox): An example of running VSCode in a sandbox.
- [**warmpool-quickstart**](./warmpool-quickstart): Reference YAML for the three extension CRDs — SandboxTemplate, SandboxWarmPool, and SandboxClaim — including a secure template and an LLM-scoped network policy example.
- [**windows-sandbox**](./windows-sandbox): An example of running a Windows guest inside the Agent Sandbox via KVM/QEMU.