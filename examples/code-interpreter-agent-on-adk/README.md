# Use Agent Sanbox code interpreter an Agent Tool.  

The guide will guide you through the process of creating a simple [ADK](https://google.github.io/adk-docs/) agent that is able to use agent sandbox as a tool.

## Installation


1. Install the Agent-Sandbox controller and CRDs to a cluster. You can follow the instructions from the [installation section from the Getting Started page](/README.md/#installation).

2. Install the Agent Sandbox [router](/clients/python/agentic-sandbox-client/README.md#setup-deploying-the-router)


3. Move into this example's folder:
   ```sh
   cd examples/code-interpreter-agent-on-adk
   ```

4. Create a Python virtual environment:
   ```sh
   python3 -m venv .venv
   source .venv/bin/activate
   ```

5. Install the dependencies:
   ```sh
   pip install -r requirements.txt
   ```

6. Set you API key to get access the Gemini model that is used in this example:
   ```sh
   export GOOGLE_API_KEY="YOUR_API_KEY"
   ```

7. Start ADK agent with the web UI:
   ```sh
   adk web
   ```

## Testing

1. Open agent's page: http://127.0.0.1:8000.

2. Tell the agent to generate some code and execute it in the sandbox:

![example](example.png)


The agent should generate the code and execute it in the agent-sandbox.


