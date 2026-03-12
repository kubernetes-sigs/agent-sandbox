# Agent Sandbox Langchain integration

The Agent Sandbox integration for the [Langchain](https://www.langchain.com/) introduces a set of framework-compatible abstractions, enabling Langchain-based projects to interact seamlessly with the Agent Sandbox. 

This page includes full code examples for:
* [Tools](#tools)

## Tools

The Agent Sandbox Langchain integration allows using sandbox as a [tool](https://docs.langchain.com/oss/python/langchain/tools#tools).

### Using Python sandbox tool 

We provide a built-in function to create a tool for a sandbox with Python environment. This example shows how to use it:

```python
from langchain_google_genai import (
    ChatGoogleGenerativeAI,
)  # pip install langchain_google_genai
from langchain.agents import create_agent

from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.langchain.tools import PythonLangChainSandboxTool

# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create a tool. The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_tool = PythonLangChainSandboxTool(sandbox_settings=sandbox_settings)

# Create and test an agent
agent = create_agent(
    # Using Gemini in this example (https://docs.langchain.com/oss/python/integrations/chat/google_generative_ai).
    # Change this in order to use another model (https://docs.langchain.com/oss/python/integrations/chat).
    model=ChatGoogleGenerativeAI(
        model="gemini-3-flash-preview",
    ),
    tools=[python_tool],
    system_prompt="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
)

result = agent.invoke(
    {"messages": [{"role": "user", "content": "Calculate 2 to the power of 64"}]}
)

for m in result["messages"]:
    m.pretty_print()
```

### Creating custom tools:

To create a new custom tool that uses Agent Sandbox, you can implement your logic in 
a function and pass it to our sandbox class:

```python
from langchain_google_genai import (
    ChatGoogleGenerativeAI,
)  # pip install langchain_google_genai
from langchain.agents import create_agent
from pydantic import Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adapter import (
    SandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)
from k8s_agent_sandbox.integrations.langchain.tools import LangChainSandboxTool


# Define an input schema for our adapter
class InputSchema(CommonBaseInputSchema):
    code: str = Field(description="The code to execute.")


# Define an adapter class.
class MyPythonSandbonExecutor(SandboxIntegrationAdapter):

    NAME = "execute_python_code_in_sandbox"
    DESCRIPTION = "Executes Python code in a sandbox and returns execution results."
    INPUT_SCHEMA = InputSchema
    RESULT_SCHEMA = CommonExecutionResultSchema

    # This is you main logic that interacts with sandbox
    # The arguments has to match the INPUT_SCHEMA arrtibute of this class
    def _execute_code(self, code: str, timeout: int = 60) -> ExecutionResult:
        with self._sandbox_settings.create_client() as sandbox:
            sandbox.write("main.py", code)
            result = sandbox.run("python3 main.py", timeout)
            return result

    # Implement this abstract method to invoke your code.
    def execute(self, **args) -> ExecutionResult:
        return self._execute_code(**args)


# Creating the LangChain tool class.
# All that we need to do is to specify the adapter class it know what to execute.
class MyPythonSandboxTool(LangChainSandboxTool):
    SANDBOX_ADAPTER_CLS = MyPythonSandbonExecutor


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)
# The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
my_coding_tool = MyPythonSandboxTool(sandbox_settings=sandbox_settings)


# Create and test an agent
agent = create_agent(
    # Using Gemini in this example (https://docs.langchain.com/oss/python/integrations/chat/google_generative_ai).
    # Change this in order to use another model (https://docs.langchain.com/oss/python/integrations/chat).
    model=ChatGoogleGenerativeAI(
        model="gemini-3-flash-preview",
    ),
    tools=[my_coding_tool],
    system_prompt="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
)

result = agent.invoke(
    {"messages": [{"role": "user", "content": "Calculate 2 to the power of 64"}]}
)

for m in result["messages"]:
    m.pretty_print()
```
