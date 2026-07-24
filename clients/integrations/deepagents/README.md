# deepagents-k8s-agent-sandbox

LangChain DeepAgents backend for Kubernetes `agent-sandbox`. This package allows you to run `deepagents` in a secure, isolated Kubernetes sandbox environment instead of running locally.

## Quick Install

```bash
pip install deepagents-k8s-agent-sandbox
```

## Basic Usage

```python
from k8s_agent_sandbox.sandbox_client import SandboxClient
from deepagents_k8s_agent_sandbox import K8sAgentSandbox

client = SandboxClient()

sandbox = K8sAgentSandbox.from_existing_claim_name(
    client,
    claim_name="some-claim",
    namespace="default"
)

result = sandbox.execute("echo hello")
print(result.output)
```

## Full Graph Example

Here is a full example of creating a `deepagents` agent graph that relies on the `K8sAgentSandbox` backend. In this example, the agent creates a python script, executes it securely in the sandbox, and returns the result.

```python
import os
from k8s_agent_sandbox import SandboxClient
from deepagents_k8s_agent_sandbox import K8sAgentSandbox
from deepagents_k8s_agent_sandbox.settings import K8sAgentSandboxSettings
from deepagents import create_deep_agent
from langchain_google_genai import ChatGoogleGenerativeAI

# 1. Setup the chat model (e.g., using LangChain with Gemini)
model = ChatGoogleGenerativeAI(model="gemini-3-pro")

# 2. Connect to the Kubernetes Sandbox cluster
client = SandboxClient()

# 3. Configure the sandbox settings
# 'warmpool' references a pre-defined SandboxTemplate in your cluster
settings = K8sAgentSandboxSettings(
    warmpool="python-deepagent",
    namespace="default"
)

# 4. Initialize the sandbox backend
# This either creates a new sandbox claim or reuses an existing one
# that matches the "scope" labels for isolation.
backend = K8sAgentSandbox.from_labels_scope(
    client=client,
    sandbox_settings=settings,
    scope={"thread": "my-graph-thread"},
)

# 5. Create the DeepAgent graph, injecting the K8s sandbox as the backend
agent = create_deep_agent(
    model=model,
    backend=backend,
    # (Optional) pass the path to custom skills:
    # skills=["./skills/python-scripting"]
)

# 6. Invoke the agent with a task that requires sandbox execution
query = "Write a python script that prints 'Hello from Sandbox', run it, and tell me the output."

print(f"User: {query}\n")
response = agent.invoke({
    "messages": [("user", query)]
})

# 7. Print the results from the agent
for msg in response.get("messages", []):
    if msg.type == "ai":
        print(f"Agent: {msg.content}")
```
