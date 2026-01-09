# Python Runtime Sandbox for Gemini Computer Use Agent

This example implements a simple Python server in a sandbox container that can run the `computer-use-preview` agent.
It includes a FastAPI server that can execute browser tasks and a Python script to test it (`tester.py`).

**Setup**

To run the agent, you need to provide a Gemini API key. You can do this by setting the `GEMINI_API_KEY` environment variable.

```bash
export GEMINI_API_KEY="YOUR_GEMINI_API_KEY"
```

You also need to install the required Python libraries:
```bash
pip install kubernetes requests
```

**Prerequisites**

The Runtime is based on the `computer-use-preview` repository, you can clone it or Docker build will do it automatically:
```bash
git clone https://github.com/google-gemini/computer-use-preview
```

## Running the Docker Test Script (`run-test-docker.sh`)

The `run-test-docker.sh` script provides a way to build and test the sandboxed agent locally using Docker. It supports both non-interactive and interactive modes.

### Flags

- `--interactive`: Runs the script in interactive mode.
- `--nobuild`: Skips the Docker image build step.

### Non-Interactive Mode (Default)

By default, the script runs in non-interactive mode. It will:
1.  Build the Docker image (unless `--nobuild` is specified).
2.  Start the container in the background.
3.  Run the `tester.py` script, which sends a predefined query to the agent running in the container.
4.  Stop and remove the container.

To run in non-interactive mode:
```bash
./run-test-docker.sh
```

The `tester.py` script acts as a client to interact with the python API server, sending a query to the `/agent` endpoint and printing the standard output, standard error, and exit code from the response.

Usage:
`python tester.py [ip] [port]`

### Interactive Mode

You can run the script in interactive mode by using the `--interactive` flag. This is useful for running custom queries against the agent inside the container. In this mode, the script will:
1.  Build the Docker image (unless `--nobuild` is specified).
2.  Start the container in the background.
3.  Execute a sample query (`python computer-use-preview/main.py --query "Go to Google and type 'Hello World' into the search bar"`) inside the container, attaching your terminal to it.

To run in interactive mode:
```bash
./run-test-docker.sh --interactive
```
To run in interactive mode without rebuilding the image:
```bash
./run-test-docker.sh --nobuild --interactive
```

## Python Classes in `main.py`

The `main.py` file defines the following Pydantic models to ensure type-safe data for the API endpoints:

### `AgentQuery`
This class models the request body for the `/agent` endpoint.
- **`query: str`**: The natural language query for the browser agent to execute.
- **`api_key: str`**: The Gemini API key for authenticating with the agent.

### `AgentResponse`
This class models the response body for the `/agent` endpoint. 
- **`stdout: str`**: The standard output from the agent execution.
- **`stderr: str`**: The standard error from the agent execution.
- **`exit_code: int`**: The exit code of the agent execution.

## Testing on a local kind cluster using agent-sandbox

To test the sandbox on a local [kind](https://kind.sigs.k8s.io/) cluster, you can use the `run-test-kind.sh` script. This script automates the entire process of setting up a local Kubernetes cluster, deploying the sandbox, and running the integration tests.

This script will:
1.  Create a kind cluster (if it doesn't exist).
2.  Build and deploy the agent sandbox controller to the cluster.
3.  Build the python runtime sandbox image.
4.  Load the image into the kind cluster.
5.  Deploy the sandbox and run the `test_computer_use_extension.py` integration tests.
6.  Clean up all the resources.

To run the script from the project root:
```bash
./examples/gemini-cu-sandbox/run-test-kind.sh
```