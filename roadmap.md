## Roadmap

High-level overview of our main strategic priorities for 2026:
- Overhaul Documentation - Restructure and expand current documentation to lower the barrier to entry for new users.
- Website Refresh [[#166](https://github.com/kubernetes-sigs/agent-sandbox/issues/166)] - Update the website to accurately reflect the latest features, documentation links, and usage examples. 
- PyPI Distribution  [[#146](https://github.com/kubernetes-sigs/agent-sandbox/issues/146)] - Publish the agent-sandbox-client package to pip for easier installation
- Expand SDK functionality - natively support methods like read, write, run_code, etc. within the Python SDK
- Benchmarking Guide
- Strict Sandbox-to-Pod Mapping [[#127]](https://github.com/kubernetes-sigs/agent-sandbox/issues/127) - Ensure a reliable 1-to-1 mapping exists between a Sandbox and a Pod
- Expand Sandbox use cases - Computer use case, browser use case, and base images
- Decouple API from Runtime - enable full customization of runtime environment without breaking API
- Implement GO Client [[#227](https://github.com/kubernetes-sigs/agent-sandbox/issues/227)]
- Scale-down / Resume PVC based - Pause resume preserving PVC only, when replicas scale to 0, PVC is saved, when sandbox scales back PVC is restored
- Add complete CR, SDK and template support
- API Support for Multi-Sandbox per Pod - Extend API to support multiple sandboxes in a Pod
- Startup Actions [[#58](https://github.com/kubernetes-sigs/agent-sandbox/issues/58)] - Allow users to specify actions at startup, like immediately pausing the sandbox or pausing it at a specific time
- Auto-deletion of (bursty) sandboxes (RL training typical usage)
- Status Updates [[#119](https://github.com/kubernetes-sigs/agent-sandbox/issues/119)] - Functionality to properly update and reflect the status of the sandbox
- Creation Latency Metrics [[#123](https://github.com/kubernetes-sigs/agent-sandbox/issues/123)] - Add a custom metric to specifically track the latency of Sandbox creation time
- Runtime API OTEL/Tracing Instrumentation - Instrument runtime API with OpenTelemetry and Tracing to provide guidance on further instrumentation
- Metadata Propagation [[#174](https://github.com/kubernetes-sigs/agent-sandbox/issues/174)] - Ensure that labels and annotations are correctly propagated to sandbox pods
- Headless Service Port Handling [[#154](https://github.com/kubernetes-sigs/agent-sandbox/issues/154)] - Ensure Headless Services correctly set ports when containerPort is configured
- Detailed logs Falco configuration extension - Propagate Falco configuration for gVisor sandbox logging. Enable configuration via Agent Sandbox API
- API Support for other isolation technologies - Continue extending the support to QEMU, Firecracker and other technologies; Process isolation (pydantic)
- OpenEnv Support [[#132](https://github.com/kubernetes-sigs/agent-sandbox/issues/132)] - Develop support for AgentSandbox within the OpenEnv environment
- Agent & RL Framework Support - Tighter integration between Agent Sandbox & popular Agent & RL frameworks like CrewAI, Ray Rllib
- Integration with kAgent
- Integration with other Sandbox offerings
- Deliver Beta/GA versions