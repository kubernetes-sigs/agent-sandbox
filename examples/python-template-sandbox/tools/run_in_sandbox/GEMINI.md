# Run in Sandbox Tool

This tool provides a simple command-line interface to execute Python code in an isolated sandbox environment for security purposes. Use this whenever you need to run and execute a python code snippet or generated code.

## How It Works

The `run` script is a wrapper around a Python script (`main.py`) that uses the `agentic-sandbox-client` to:
1.  Create a new sandbox on the Kubernetes cluster.
2.  Write the provided Python code to a file inside the sandbox.
3.  Execute the file with the Python interpreter in the sandbox.
4.  Stream the `stdout` and `stderr` back to the console.
5.  Clean up all sandbox resources automatically.

## Prerequisites

-   A running Kubernetes cluster (e.g., `kind`).
-   The Agent Sandbox controller must be deployed with the extensions feature enabled.
-   A `SandboxTemplate` named `python-sandbox-template` must be created in the cluster.
-   The `agentic-sandbox-client` and its dependencies must be installed in the environment where you are running the tool.

## Setup

1.  **Create a virtual environment:**
    ```bash
    python3 -m venv .venv
    ```
2.  **Activate the virtual environment:**
    ```bash
    source .venv/bin/activate
    ```
3.  **Install the required dependencies:**
    ```bash
    pip install -e ../../agentic-sandbox-client/
    ```
4.  **Install as an extension in gemini cli:**
    ```bash
    gemini extensions install ./tools/run_in_sandbox/
    ```

## Usage

To run a snippet of Python code, pass it as a string argument to the `run` script.

**Example:**

```bash
./run "import os; print(os.listdir())"
```

This will create a sandbox, execute the command, print the output, and then tear down the sandbox.

## Troubleshooting

**`ModuleNotFoundError: No module named 'agentic_sandbox'`**

This error occurs when the `agentic_sandbox` module is not installed in the Python environment that the `run` script is using. To fix this, make sure you have activated the virtual environment and installed the dependencies as described in the "Setup" section.

If you have already installed the dependencies, you may need to modify the `run` script to use the Python interpreter from the virtual environment. To do this, change the last line of the `run` script from:

```bash
python3 "$SCRIPT_DIR/main.py" "$1"
```

to:

```bash
"$SCRIPT_DIR/.venv/bin/python" "$SCRIPT_DIR/main.py" "$1"
```