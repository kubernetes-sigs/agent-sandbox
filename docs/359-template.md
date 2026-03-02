### 1. Summary

This KEP discusses the idea of a Unified Agent Sandbox SDK that provides AI Agent Orchestrators / Platform Admins with a Fluent API for managing remote execution environments. This design moves away from treating a sandbox as a transient Python script helper (via `ContextManager` i.e `with`/`__enter__`/`__exit__`) and instead treats it as a Persistent Resource Handle. By abstracting the sandbox into specialized engine (Execution, Filesystem etc), the SDK provides a robust interface for long-lived, stateful agentic workflows.

### 2. Motivation

Traditional Python SDKs favor the Context Manager pattern `(with Sandbox() as sbx:)`. While appropriate for short-lived scripts, this pattern is a fundamental hindrance to the long-lived, asynchronous nature of AI agent workflows. The Unified SDK abstracts the underlying infrastructure (Kubernetes CRDs) into a single logical object called `Sandbox`.

1. **Identity Stability**: The Sandbox object represents a stable identity (`sandbox_id`). Whether the underlying Pod is running, suspended, or resuming, the developer interacts with the same object.

2. **Orchestration vs. Execution**: By abstracting the object, we can separate the Management Path (lifecycle changes) from the Execution Path (running code). Although, we do provide `api_url` to connect to the sandbox, it is not a very good API model. 

3. **Capability Discovery**: Dot-notation namespacing (e.g., `sbx.files`, `sbx.process`) allows the SDK to grow in functionality without bloating the root object.

4. **Distributed ownership**: By moving to an explicit Resource Handle based on a `sandbox_id`, we allow the logical ownership of a sandbox to move across the network.

5. **Non linear Logic**: Agents often manage multiple sandboxes simultaneously (e.g., a "Researcher" sandbox and a "Coder" sandbox). Nesting `with` blocks for multiple long-lived resources leads to unreadable code and complex error-handling logic.


### API Specification

The SDK architecture is divided into "Engines" to ensure the single-responsibility principle.

#### The Core Handle (Sandbox)

The root object manages the state of the resource and holds references to specialized engines.

```python
class Sandbox:
    def __init__(self, sandbox_id, router_dns):
        self.id = sandbox_id
        # Namespaced Engines
        self.commands = CoreExecution(sandbox_id, router_dns)
        self.files = Filesystem(sandbox_id, router_dns)

    def suspend(self):
        """Hibernates the environment; saves memory/disk to CSI snapshots."""
        return self._manager.suspend(self.id)

    def resume(self):
        """Wakes the environment; rehydrates from the last snapshot."""
        return self._manager.resume(self.id)

    def terminate(self):
        """Permanent deletion of all infrastructure and state."""
        return self._manager.terminate(self.id)
```

#### Specialized Engines

Engines talk to the Sandbox Router via a stable DNS, using the X-Sandbox-Id header to maintain session persistence.

**CoreExecution (sbx.core)**: Handles run_code and run_cmd.

**FileSystem (sbx.files)**: Handles read, write, and list operations.

**ProcessSystem (sbx.process)**: Handles the creation and killing of processes inside a Sandbox. 

#### Developer Experience (The "Fluent" API)

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
