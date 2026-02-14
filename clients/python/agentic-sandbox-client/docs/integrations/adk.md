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
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.adk.tools import PythonSandboxTool


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create a tool. The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_tool = PythonSandboxTool(sandbox_settings)

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
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.adk.tools import SandboxFunctionTool


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)


def my_coding_tool(code: str, **kwargs):
    """<Tool description>"""

    # Sandbox settings are injected into the kwargs under the 'sandbox' argument.
    sandbox_settings = kwargs["sandbox"]
    with sandbox_settings.create_client() as sandbox:
        sandbox.write("main.py", code)
        result = sandbox.run("python3 main.py")

        return {
            "status": "success",
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exit_code": result.exit_code,
        }


# Create a tool from the function. The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
my_tool = SandboxFunctionTool(sandbox_settings, my_coding_tool)

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
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.adk.code_executors import PythonSandboxCodeExecutor


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create an executor. The executor will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_code_executor = PythonSandboxCodeExecutor(sandbox_settings)

# NOTE: There's an ongoing issue with code executors in ADK: https://github.com/google/adk-python/pull/3699
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
from google.adk.agents.invocation_context import InvocationContext
from google.adk.code_executors.code_execution_utils import (
    CodeExecutionInput,
    CodeExecutionResult,
)
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.adk.code_executors import SandboxCodeExecutor


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)


class MySandboxCodeExecutor(SandboxCodeExecutor):
    def execute_code(
        self,
        invocation_context: InvocationContext,
        code_execution_input: CodeExecutionInput,
    ) -> CodeExecutionResult:

        with self._sandbox_settings.create_client() as sandbox:
            sandbox.write("main.py", code_execution_input.code)
            result = sandbox.run("python3 main.py")

        return CodeExecutionResult(
            stdout=result.stdout,
            stderr=result.stderr,
            output_files=[],
        )


# Create an executor. The executor will create a sandbox according to the settings from the 'sandbox_settings' argument.
code_executor = MySandboxCodeExecutor(sandbox_settings)

# NOTE: There's an ongoing issue with code executors in ADK: https://github.com/google/adk-python/pull/3699
root_agent = Agent(
    model="gemini-3-flash-preview",
    name="custom_sanbox_code_executor_agent",
    instruction="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    code_executor=code_executor,
)
```
