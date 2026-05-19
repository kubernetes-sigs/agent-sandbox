# Agent Sandbox CrewAI integration

The Agent Sandbox integration for the [CrewAI](https://docs.crewai.com/) introduces a set of framework-compatible abstractions, enabling CrewAI-based projects to interact seamlessly with the Agent Sandbox. 

This page includes full code examples for:
* [Tools](#tools)

## Tools

The Agent Sandbox CrewAI integration allows using sandbox as a [tool](https://docs.crewai.com/en/concepts/tools).

### Using Python sandbox tool 

We provide a built-in function to create a tool for a sandbox with Python environment. This example shows how to use it:

```python
from crewai import Agent, Task, Crew

from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.crewai.tools import PythonCrewAISandboxTool

# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)
# Instantiate the tool
python_sandbox_tool = PythonCrewAISandboxTool(sandbox_settings=sandbox_settings)

editor_agent = Agent(
    llm="gemini/gemini-3-flash-preview",
    role="Senior Software Engineer",
    goal="Execute code in a sandbox",
    backstory="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    tools=[python_sandbox_tool],
    verbose=True,
    memory=True,
)

analysis_task = Task(
    description="Write a code that calculates 2 to the power of 64.",
    expected_output="A report containing the exact word count and a brief sentiment analysis.",
    agent=editor_agent,
)

crew = Crew(
    agents=[editor_agent], tasks=[analysis_task], process="sequential", verbose=True
)

print("### Starting Crew Execution ###")
result = crew.kickoff()

print("\n\n########################")
print("## Final Result ##")
print("########################\n")
print(result)
```

### Creating custom tools:

To create a new custom tool that uses Agent Sandbox, you can implement your logic in 
a function and pass it to our sandbox class:

```python
from crewai import Agent, Task, Crew
from pydantic import Field

from k8s_agent_sandbox.sandbox_client import ExecutionResult
from k8s_agent_sandbox.integrations import SandboxSettings
from k8s_agent_sandbox.integrations.adapter import (
    SandboxIntegrationAdapter,
    CommonBaseInputSchema,
    CommonExecutionResultSchema,
)
from k8s_agent_sandbox.integrations.crewai.tools import BaseCrewAISandboxTool


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


# Creating the CrewAI tool class.
# All that we need to do is to specify the adapter class it know what to execute.
class MyPythonSandboxTool(BaseCrewAISandboxTool):
    SANDBOX_ADAPTER_CLS = MyPythonSandbonExecutor


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)
# The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
my_coding_tool = MyPythonSandboxTool(sandbox_settings=sandbox_settings)

editor_agent = Agent(
    llm="gemini/gemini-3-flash-preview",
    role="Senior Software Engineer",
    goal="Execute code in a sandbox",
    backstory="You are a helpful agent that can write and execute code in a sandbox environment to answer questions and solve problems.",
    tools=[my_coding_tool],
    verbose=True,
    memory=True,
)

analysis_task = Task(
    description="Write a code that calculates 2 to the power of 64.",
    expected_output="A report containing the exact word count and a brief sentiment analysis.",
    agent=editor_agent,
)

crew = Crew(
    agents=[editor_agent], tasks=[analysis_task], process="sequential", verbose=True
)

print("### Starting Crew Execution ###")
result = crew.kickoff()

print("\n\n########################")
print("## Final Result ##")
print("########################\n")
print(result)
```
