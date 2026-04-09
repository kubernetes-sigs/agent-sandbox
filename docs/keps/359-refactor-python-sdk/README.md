### 1. Summary

This KEP discusses the idea of a Unified Agent Sandbox SDK that provides AI Agent Orchestrators / Platform Admins with a Fluent API for managing remote execution environments. This design moves away from treating a sandbox as a transient Python script helper (via `ContextManager` i.e `with`/`__enter__`/`__exit__`) and instead treats it as a Persistent Resource Handle. By abstracting the sandbox into specialized engine (Execution, Filesystem etc), the SDK provides a robust interface for long-lived, stateful agentic workflows.

### 2. Motivation

Traditional Python SDKs favor the Context Manager pattern `(with Sandbox() as sbx:)`. While appropriate for short-lived scripts, this pattern is a fundamental hindrance to the long-lived, asynchronous nature of AI agent workflows. The Unified SDK abstracts the underlying infrastructure (Kubernetes CRDs) into a single logical object called `Sandbox`.

1. **Identity Stability**: The Sandbox object represents a stable identity (`sandbox_id`). Whether the underlying Pod is running, suspended, or resuming, the developer interacts with the same object.

2. **Orchestration vs. Execution**: By abstracting the object, we can separate the Management Path (lifecycle changes) from the Execution Path (running code). Although, we do provide `api_url` to connect to the sandbox with the current `with` API model, it is not very intuitive for the customer. 

3. **Capability Discovery**: Dot-notation namespacing (e.g., `sbx.files`, `sbx.process`) allows the SDK to grow in functionality without bloating the root object.

4. **Distributed ownership**: By moving to an explicit Resource Handle based on a `sandbox_id`, we allow the logical ownership of a sandbox to move across the network.

5. **Non linear Logic**: Agents often manage multiple sandboxes simultaneously (e.g., a "Researcher" sandbox and a "Coder" sandbox). Nesting `with` blocks for multiple long-lived resources leads to unreadable code and complex error-handling logic.


### 3. API Specification

The SDK architecture is divided into three tiers: the Entry Client, the Resource Handle, and the specialized Engines.

#### 3.1 The EntryPoint (`SandboxClient`)

The `SandboxClient` handles global configuration and acts as a factory for sandboxes. It encapsulates the connection to the control plane.

```python
class SandboxClient:
    """Entry point for the SDK. Manages configuration and sandbox creation."""
    def __init__(self,router_dns: str):
        self.router_dns = router_dns

    def create_sandbox(self, template: str, namespace: str = "default") -> Sandbox:
        """Provisions a new sandbox and returns a Resource Handle."""
        return Sandbox("sandbox_id", self.router_dns, self)

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """Re-attaches to an existing sandbox by ID."""
        return Sandbox(sandbox_id, self.router_dns, self)
```

#### 3.2 The Core Handle (Sandbox)

The root object manages the state of the resource and holds references to specialized engines.

```python
class Sandbox:
    def __init__(self, sandbox_id, router_dns):
        self.id = sandbox_id
        # Namespaced Engines
        self.commands = CoreExecution(sandbox_id, router_dns)
        self.files = Filesystem(sandbox_id, router_dns)

    def status(self):
        """Fetches the current lifecycle state from the helper."""
        return self._helper.status(self.id) # This helper internally calls the K8 APIs. 
        
    def suspend(self):
        """Hibernates the environment; saves memory/files to Storage."""
        return self._helper.suspend(self.id)

    def resume(self):
        """Wakes the environment; rehydrates from the last snapshot."""
        return self._helper.resume(self.id)

    def terminate(self):
        """Permanent deletion of all infrastructure and state."""
        return self._helper.terminate(self.id)
```

#### 3.3 Specialized Engines

Engines talk to the Sandbox Router via a stable DNS, using the `X-Sandbox-Id` header to maintain session persistence.

*CoreExecution (sbx.core)*: Handles stateless and stateful `run_code` and `run_cmd`.

*FileSystem (sbx.files)*: Handles read, write, and list operations on files.

*ProcessSystem (sbx.process)*: Handles the creation and killing of processes inside a Sandbox. 

#### 3.4 Developer Experience (The "Fluent" API)

The final result is a library that feels like a native extension of the Agent's brain:

```python
# Initialize the entry point
client = SandboxClient(router_dns="router.sandbox.svc")

# Provision - No context manager used
sbx = client.create_sandbox(template="python-ml")

# Use modular engines
sbx.files.write("data.py", "x = 42")
sbx.core.run_code("import data; print(data.x)")

# Explicitly suspend when done with the current task phase
sbx.suspend()

# Re-attach later (even in a different process)
old_sbx = client.get_sandbox("sbx_123")
old_sbx.resume()
```

### 4. Proof Of Concept

https://github.com/kubernetes-sigs/agent-sandbox/pull/365

1. Implemented the changes proposed above. The session to router is initiated as part of Sandbox instance creation.
2. Updated the test client to support the resource handle way of sandbox creation.

### 5. Scalability

1. In future, if we want to manage the creation and management
of the Sandboxes via a custom Sandbox manager, we can easily
integrate that in the `Sandbox` resource handle. 
2. Sandbox Manager POC: https://github.com/SHRUTI6991/agent-sandbox-initial-playing/tree/two-clients/clients/python/agentic-sandbox-client/sandbox-manager. 