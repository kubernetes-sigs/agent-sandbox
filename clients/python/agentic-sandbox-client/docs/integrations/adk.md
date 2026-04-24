# Agent Sandbox ADK integration

The Agent Sandbox integration for the [Agent Development Kit (ADK)](https://google.github.io/adk-docs/) introduces a set of framework-compatible abstractions, enabling ADK-based projects to interact seamlessly with the Agent Sandbox. 

This page includes full code examples for:
* [Tools](#tools)
* [Code Executors](#code-executors)

## Tools

The Agent Sandbox ADK integration allows using sandbox as a [tool](https://google.github.io/adk-docs/tools/).

### Using Python sandbox tool 

We provide a built-in tool class for a sandbox with Python environment. This example shows how to use it:

```python
from google.adk.agents.llm_agent import Agent

from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adk.tools import PythonADKSandboxTool

# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create a tool. The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_tool = PythonADKSandboxTool(sandbox_settings)

root_agent = Agent(
    model="gemini-3-flash-preview",
    name="python_sandbox_tool_agent",
    instruction="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    tools=[python_tool],
)
```

### Creating custom tools:

To create a new custom tool that uses Agent Sandbox, you can implement your logic in 
a function and pass it to our sandbox class:

```python
from google.adk.agents.llm_agent import Agent
from pydantic import Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adapter import (
    SandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)
from k8s_agent_sandbox.integrations.adk.tools import BaseADKSandboxTool


# Define an input schema for our adapter
class InputSchema(CommonBaseInputSchema):
    code: str = Field(description="The code to execute.")


# Define an adapter class.
class MyPythonSandboxAdapter(SandboxIntegrationAdapter):
    NAME = "execute_python_code_in_sandbox_custom"
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


# Creating the ADK tool class.
# All that we need to do is to specify the adapter class it know what to execute.
class MyPythonSandboxTool(BaseADKSandboxTool):
    SANDBOX_ADAPTER_CLS = MyPythonSandboxAdapter


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
my_tool = MyPythonSandboxTool(sandbox_settings=sandbox_settings)

root_agent = Agent(
    model="gemini-3-flash-preview",
    name="custom_sandbox_tool_agent",
    instruction="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    tools=[my_tool],
)
```

## Code Executors

The Agent Sandbox ADK integration allows using sandbox as a [code executor](https://google.github.io/adk-docs/tools/gemini-api/code-execution/).

### Using Python sandbox code executor 

We provide a built-in code executor class for a sandbox with Python environment. This example shows how to use it:

```python
from google.adk.agents.llm_agent import Agent
from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adk.code_executors import (
    PythonADKSandboxCodeExecutor,
)

# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create an executor. The executor will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_code_executor = PythonADKSandboxCodeExecutor(sandbox_settings=sandbox_settings)

root_agent = Agent(
    model="gemini-3-flash-preview",
    name="python_code_executor_agent",
    instruction="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    code_executor=python_code_executor,
)
```

### Creating custom code executors:

To create a new custom code executor that uses Agent Sandbox, you can subclass our base code executor class and implement your logic. 

Here is a complete example:

```python
from google.adk.agents.llm_agent import Agent
from pydantic import Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adapter import (
    SandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)
from k8s_agent_sandbox.integrations.adk.code_executors import (
    ADKSandboxCodeExecutor,
)


# Define an input schema for our adapter
class InputSchema(CommonBaseInputSchema):
    code: str = Field(description="The code to execute.")


# Define an adapter class.
class MyPythonSandbonAdapter(SandboxIntegrationAdapter):

    NAME = "execute_python_code_in_sandbox_custom"
    DESCRIPTION = "Executes Python code in a sandbox and returns execution results."
    INPUT_SCHEMA = InputSchema
    RESULT_SCHEMA = CommonExecutionResultSchema

    # This is you main logic that interacts with a sandbox
    # The arguments has to match the INPUT_SCHEMA arrtibute of this class
    def _execute_code(self, code: str, timeout: int = 60) -> ExecutionResult:
        with self._sandbox_settings.create_client() as sandbox:
            sandbox.write("main.py", code)
            result = sandbox.run("python3 main.py", timeout)
            return result

    # Implement this abstract method to invoke your code.
    def execute(self, **args) -> ExecutionResult:
        return self._execute_code(**args)


# Creating the ADK Code Executor subclass.
class MyPythonSandboxCodeExecutor(ADKSandboxCodeExecutor):
    # Specify our previously defined adapter to know what to execute.
    SANDBOX_ADAPTER_CLS = MyPythonSandbonAdapter

    # Implement this abstract method to execute the adapter.
    def _execute_code(self, code: str, timeout: int = 60) -> ExecutionResult:
        return self._adapter.execute(code=code, timeout=timeout)


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create an executor. The executor will create a sandbox according to the settings from the 'sandbox_settings' argument.
code_executor = MyPythonSandboxCodeExecutor(sandbox_settings=sandbox_settings)

root_agent = Agent(
    model="gemini-3-flash-preview",
    name="custom_sanbox_code_executor_agent",
    instruction="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    code_executor=code_executor,
)
```
