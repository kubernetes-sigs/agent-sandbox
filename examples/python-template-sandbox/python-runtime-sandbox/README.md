# Python Runtime Sandbox

This example implements a simple Python server in a sandbox container. 
It includes a FastAPI server that can execute commands and a Python script to test it (`tester.py`).

Test it out by running `run-test-docker`:
It will build a (local) container image containing the python server,run it, then execute `tester.py` to test the running container and a cleanup.

The `tester.py` script acts as a client to interact with the python API server, sending a command to the `/execute` endpoint and printing the standard output, standard error, and exit code from the response.

Usage:
`python tester.py [ip] [port]`

## Python Classes in `main.py`

The `main.py` file defines the following Pydantic models to ensure type-safe data for the API endpoints:

### `ExecuteRequest`
This class models the request body for the `/execute` endpoint.
- **`command: str`**: The shell command to be executed in the sandbox.

### `ExecuteResponse`
This class models the response body for the `/execute` endpoint.
- **`stdout: str`**: The standard output from the executed command.
- **`stderr: str`**: The standard error from the executed command.
- **`exit_code: int`**: The exit code of the executed command.

## Testing on a local kind cluster using agent-sandbox

To test the sandbox on a local [kind](https://kind.sigs.k8s.io/) cluster, you can use the `run-test-kind.sh` script.
This script will:
1.  Create a kind cluster (if it doesn't exist).
2.  Build and deploy the agent sandbox controller to the cluster.
3.  Build the python runtime sandbox image.
4.  Load the image into the kind cluster.
5.  Deploy the sandbox and run the tests using examples/python-runtime-sandbox/sandbox-python-kind.yaml
6.  Clean up all the resources.

To run the script:
```bash
./run-test-kind.sh
```
