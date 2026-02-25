# Agent Sandbox Langchain integration

The Agent Sandbox integration for the [Langchain](https://www.langchain.com/) introduces a set of framework-compatible abstractions, enabling Langchain-based projects to interact seamlessly with the Agent Sandbox. 

This page includes full code examples for:
* [Tools](#tools)

## Tools

The Agent Sandbox Langchain integration allows using sandbox as a [tool](https://docs.langchain.com/oss/python/langchain/tools#tools).

### Using Python sandbox tool 

We provide a built-in function to create a tool for a sandbox with Python environment. This example shows how to use it:

```python
from langchain_google_genai import ChatGoogleGenerativeAI # pip install langchain_google_genai
from langchain.agents import create_agent
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.langchain.tools import create_python_sandbox_tool


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)

# Create a tool. The tool will create a sandbox according to the settings from the 'sandbox_settings' argument.
python_tool = create_python_sandbox_tool(sandbox_settings)

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
from langchain_google_genai import ChatGoogleGenerativeAI # pip install langchain_google_genai

from langchain.agents import create_agent
from agentic_sandbox.integrations import SandboxSettings
from agentic_sandbox.integrations.langchain.tools import sandbox_tool


# Specify sandbox specific settings in the sandbox settings instance.
sandbox_settings = SandboxSettings(
    template_name="python-sandbox-template",
    namespace="default",
)


# Create a tool by using a 'sandbox_tool' decorator.
# The tool will create a sandbox according to the settings from the 'sandbox_settings' argument which is passed to the decorator.
@sandbox_tool(sandbox_settings)
def my_coding_tool(code: str, **kwargs):
    """<Tool description>"""

    # the 'sandbox_tool' injects the sandbox settings into the 'sandbox' keyword argument.
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
