# Agentic Sandbox Computer Use Extension

This directory contains the Python client extension for interacting with the Agentic Sandbox in a "computer use" scenario. This extension allows programmatic control of a sandbox environment that can execute actions within a virtual desktop, such as browsing the web or running shell commands.

## `computer_use.py`

This file defines the `ComputerUseSandbox` class, which is a specialized client for the computer-use example. It extends the base `SandboxClient` to provide an `agent` method.

### Key Features:

*   **`ComputerUseSandbox(template_name: str, namespace: str = "default", server_port: int = 8080)`**: 
    *   Initializes the client, specifying the Kubernetes SandboxTemplate to use, its namespace, and the port for the sandbox router service.
*   **`agent(self, query: str, api_key: str | None = None, timeout: int = 60) -> ExecutionResult`**: 
    *   Executes a natural language `query` using an agent within the sandbox. It requires an `api_key` for authentication. If no API key is provided, it returns an `ExecutionResult` with an error. The method sends the query to the sandbox router and returns the `stdout`, `stderr`, and `exit_code` from the executed agent task.

## `test_computer_use_extension.py`

This file contains the unit tests for the `ComputerUseSandbox` extension. It ensures that the extension correctly interacts with the sandbox environment and handles various scenarios, including the presence or absence of an API key.

### Key Tests:

*   **`setUp(self)`**: This method runs before each test. It's responsible for ensuring the `GEMINI_API_KEY` environment variable is set and creates a Kubernetes secret named `gemini-api-key` using this value. This secret is crucial for the sandbox's agent to authenticate with external services.
*   **`test_agent_with_api_key(self)`**: This test verifies the `agent` method's functionality when a valid API key is provided. It sends a sample query (e.g., navigating to a website and extracting information) and asserts that the `exit_code` is 0 (success) and the expected output is present in `stdout`.
*   **`test_agent_without_api_key(self)`**: This test ensures that the `agent` method correctly handles scenarios where no API key is provided. It expects an `exit_code` of 1 (failure) and an "API key" related error message in `stderr`.

### Prerequisites

Before running the tests, you must have the following set up:

1.  **Python Virtual Environment**: It is highly recommended to use a virtual environment to manage dependencies.
    ```bash
    # From the project root directory
    python3 -m venv .venv
    source .venv/bin/activate
    ```

2.  **Install Needed Dependencies**: Install the required Python packages, including the local `agentic_sandbox` client in editable mode.
    ```bash
    pip install kubernetes requests
    pip install -e clients/python/agentic-sandbox-client/
    ```

3.  **Apply Sandbox Configuration**: The necessary agent-sandbox controller, extensions, including the `sandbox-router` deployment, must be applied to your Kubernetes cluster.
4.  **Apply the Gemini computer-use runtime configuration** Please refer to `examples/gemini-cu-sandbox` to see prerequsities and apply the correct `SandboxTemplate`.
    ```bash
    kubectl apply -f examples/gemini-cu-sandbox/sandbox-gemini-computer-use.yaml
    ```

### Running Tests:

Once the prerequisites are met, you can use the `extensions.computer_use.py` to interact with the runtime and run the tests from the project root directory.

Example:
```bash
python3 -m unittest clients.python.agentic-sandbox-client.test_computer_use_extension
```
