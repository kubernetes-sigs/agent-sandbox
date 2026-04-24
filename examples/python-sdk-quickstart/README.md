# Python SDK Quickstart

Agent Sandbox is a quick and easy way to start secure containers that will let agents run, execute code, call tools and interact with data. Using the SDK users can easily interact with the sandboxes without using Kubernetes primitives.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller](/README.md/#installation) installed.
- The [Sandbox Router](/clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router) deployed in your cluster.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox](/examples/python-runtime-sandbox/README.md) guide for setup instructions.
- The [Python SDK](/clients/python/agentic-sandbox-client/README.md) installed: `pip install k8s-agent-sandbox`.

## Connection Modes

`SandboxClient()` with no arguments defaults to **Tunnel mode** (`SandboxLocalTunnelConnectionConfig`), which opens a `kubectl port-forward` tunnel to the Router Service — no public IP required, works on KinD and Minikube.

The SDK supports three modes:

| Mode | Config class | When to use |
|------|-------------|-------------|
| **Tunnel** (default) | `SandboxLocalTunnelConnectionConfig` | Local development and CI — tunnels via `kubectl port-forward` |
| **Gateway** | `SandboxGatewayConnectionConfig` | Production clusters with a public Kubernetes Gateway |
| **Direct** | `SandboxDirectConnectionConfig` | In-cluster agents or custom domains, bypasses discovery entirely |

## Usage

Start with a simple run command:

```python
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()

sandbox = client.create_sandbox(
    template="python-sandbox-template",
    namespace="default",
)
try:
    result = sandbox.commands.run("echo 'Hello from Agent Sandbox!'")
    print(result.stdout)
    # Hello from Agent Sandbox!
finally:
    sandbox.terminate()
```

Or write a file into the sandbox filesystem, then read it:

```python
sandbox = client.create_sandbox(
    template="python-sandbox-template",
    namespace="default",
)
try:
    sandbox.files.write(
        "hello.py",
        'print("Hello, World! Greetings from inside the sandbox.")\n',
    )
    result = sandbox.commands.run("python3 hello.py")
    print(result.stdout)
    # Hello, World! Greetings from inside the sandbox.
finally:
    sandbox.terminate()
```


## References

- [Python SDK documentation](../../clients/python/agentic-sandbox-client/) — full API reference and connection modes.
- [Using Agent Sandbox as a Tool in ADK](../code-interpreter-agent-on-adk/) — integrate sandboxes into an AI agent.