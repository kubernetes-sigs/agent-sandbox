from google.adk.agents.llm_agent import Agent
from agentic_sandbox import SandboxClient


def execute_python(code: str):
    with SandboxClient(
        template_name="python-sandbox-template",
        namespace="default"
    ) as sandbox:
        sandbox.write("run.py", code)
        result = sandbox.run("python3 run.py")
        return result.stdout


root_agent = Agent(
    model='gemini-2.5-flash',
    name='coding_agent',
    description="Writes Python code and executes it in a sandbox.",
    instruction="You are a helpful assistant that can write Python code and execute it in the sandbox. Use the 'execute_python' tool for this purpose.",
    tools=[execute_python],
)
