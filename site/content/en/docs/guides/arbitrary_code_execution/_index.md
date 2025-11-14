---
title: "Arbitrary Code Execution"
linkTitle: "Arbitrary Code Execution"
weight: 2
description: >
  This guide shows how to deploy a simple Python server that executes arbitrary commands in a sandbox container.
---

The server application includes a FastAPI server that can execute commands that are sent to it through HTTP requests.

## Python Classes of the server app

The `examples/python-runtime-sandbox/main.py` file defines the following Pydantic models to ensure type-safe data for the API endpoints:

### `ExecuteRequest`
This class models the request body for the `/execute` endpoint.
- **`command: str`**: The shell command to be executed in the sandbox.

### `ExecuteResponse`
This class models the response body for the `/execute` endpoint.
- **`stdout: str`**: The standard output from the executed command.
- **`stderr: str`**: The standard error from the executed command.
- **`exit_code: int`**: The exit code of the executed command.
## Install Agent Sandbox on a local Kind cluster

In this example we will create a [Kind (Kubernetes In Docker)](https://kind.sigs.k8s.io/) cluster to install the Agent Sandbox.

1. Clone the `agent-sandbox` repository if needed:

   ```sh
   git clone https://github.com/kubernetes-sigs/agent-sandbox.git
   ```

2. Move to the repository folder:

   ```sh
   cd agent-sandbox
   ```

3. Create a Kind cluster and deploy the agent controller by following this [installation tutorial](../../installation/_index.md).

## Deploy Python Runtime Sandbox

1. Go to the Python Runtime example folder:

   ```sh
   cd examples/python-runtime-sandbox
   ```

2. Build image with Python Runtime

   ```sh
   docker build -t sandbox-runtime .
   ```

3. Load the resulting image into the Kind cluster:

   ```sh
   kind load docker-image sandbox-runtime:latest --name agent-sandbox
   ```

4. Apply Python runtime sandbox CRD and deployment:

   ```sh
   kubectl apply -f sandbox-python-kind.yaml
   ```

5. Wait for the sandbox pod to be ready:

   ```sh
   kubectl wait --for=condition=ready pod --selector=sandbox=my-python-sandbox --timeout=60s
   ```

## Test runtime sandbox

1. Create another terminal session and port-forward sandbox’s pod in order to access it:

   ```sh
   kubectl port-forward "pod/sandbox-python-example" 8888:8888
   ```

2. Verify that runtime sandbox’s server is up:

   ```sh
   curl  127.0.0.1:8888/
   ```

   The output should be similar to:

   ```log
   {"status":"ok","message":"Sandbox Runtime is active."}
   ```

3. Create an environment variable with the command that has to be executed:

   ```sh
   PAYLOAD="{\"command\": \"echo 'hello world'\"}"
   ```

4. Execute the command:

   ```sh
   curl -X POST -H "Content-Type: application/json" -d "${PAYLOAD}" 127.0.0.1:8888/execute
   ```

   The output should be similar to:

   ```log
   {"stdout":"hello world\n","stderr":"","exit_code":0}
   ```

## Cleanup

1. Delete the Kind cluster:

   ```sh
   kind delete cluster --name agent-sandbox
   ```
