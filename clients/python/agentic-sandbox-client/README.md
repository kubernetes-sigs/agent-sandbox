# Agentic Sandbox Client Python

This Python client provides a simple, high-level interface for creating and interacting with
sandboxes managed by the Agent Sandbox controller. It's designed to be used as a context manager,
ensuring that sandbox resources are properly created and cleaned up.

It supports a **scalable, cloud-native architecture** using Kubernetes Gateways and a specialized
Router, while maintaining a convenient **Developer Mode** for local testing.

## Architecture

The client operates in two modes:

1.  **Production (Gateway Mode):** Traffic flows from the Client -> Cloud Load Balancer (Gateway)
    -> Router Service -> Sandbox Pod. This supports high-scale deployments.
2.  **Development (Tunnel Mode):** Traffic flows from Localhost -> `kubectl port-forward` -> Router
    Service -> Sandbox Pod. This requires no public IP and works on Kind/Minikube.
3.  **Advanced / Internal Mode**: The client connects directly to a provided api_url, bypassing
    discovery. This is useful for in-cluster communication or when connecting through a custom domain.

## Prerequisites

- A running Kubernetes cluster.
- The **Agent Sandbox Controller** installed.
- `kubectl` installed and configured locally.

## Setup: Deploying the Router

Before using the client, you must deploy the `sandbox-router`. This is a one-time setup.

1.  **Build and Push the Router Image:**

    For both Gateway Mode and Tunnel mode, follow the instructions in [sandbox-router](sandbox-router/README.md)
    to build, push, and apply the router image and resources.

2.  **Create a Sandbox Template:**

    Ensure a `SandboxTemplate` exists in your target namespace. The [test_client.py](test_client.py)
    uses the [python-runtime-sandbox](../../../examples/python-runtime-sandbox/) image.

    ```bash
    kubectl apply -f python-sandbox-template.yaml
    ```

## Installation

1.  **Create a virtual environment:**

    ```bash
    python3 -m venv .venv
    source .venv/bin/activate
    ```

2.  **Option 1: Install from source via git:**

    ```bash
    # Replace "main" with a specific version tag (e.g., "v0.1.0") from
    # https://github.com/kubernetes-sigs/agent-sandbox/releases to pin a version tag.
    export VERSION="main"

    pip install "git+https://github.com/kubernetes-sigs/agent-sandbox.git@${VERSION}#subdirectory=clients/python/agentic-sandbox-client"
    ```

2.  **Option 2: Install from source in editable mode:**

    If you have not already done so, first clone this repository:

    ```bash
    cd ~
    git clone https://github.com/kubernetes-sigs/agent-sandbox.git
    cd agent-sandbox/clients/python/agentic-sandbox-client
    ```

    And then install the agentic-sandbox-client into your activated .venv:

    ```bash
    pip install -e .
    ```

    If you are using [tracing](#tracing-with-open-telemetry), install with the optional tracing
    dependencies:

    ```
    pip install .[tracing]
    ```

## Usage Examples

### 1. Production Mode (GKE Gateway)

Use this when running against a real cluster with a public Gateway IP. The client automatically
discovers the Gateway.

```python
from agentic_sandbox import SandboxClient

# Connect via the GKE Gateway
with SandboxClient(
    template_name="python-sandbox-template",
    gateway_name="external-http-gateway",  # Name of the Gateway resource
    namespace="default"
) as sandbox:
    print(sandbox.run("echo 'Hello from Cloud!'").stdout)
```

### 2. Developer Mode (Local Tunnel)

Use this for local development or CI. If you omit `gateway_name`, the client automatically opens a
secure tunnel to the Router Service using `kubectl`.

```python
from agentic_sandbox import SandboxClient

# Automatically tunnels to svc/sandbox-router-svc
with SandboxClient(
    template_name="python-sandbox-template",
    namespace="default"
) as sandbox:
    print(sandbox.run("echo 'Hello from Local!'").stdout)
```

### 3. Advanced / Internal Mode

Use `api_url` to bypass discovery entirely. Useful for:

- **Internal Agents:** Running inside the cluster (connect via K8s DNS).
- **Custom Domains:** Connecting via HTTPS (e.g., `https://sandbox.example.com`).

```python
with SandboxClient(
    template_name="python-sandbox-template",
    # Connect directly to a URL
    api_url="http://sandbox-router-svc.default.svc.cluster.local:8080",
    namespace="default"
) as sandbox:
    sandbox.run("ls -la")
```

### 4. Custom Ports

If your sandbox runtime listens on a port other than 8888 (e.g., a Node.js app on 3000), specify `server_port`.

```python
with SandboxClient(
    template_name="node-sandbox-template",
    server_port=3000
) as sandbox:
    # ...
```

## Testing

A test script is included to verify the full lifecycle (Creation -> Execution -> File I/O -> Cleanup).

### Run in Dev Mode:

```
python test_client.py --namespace default
```

### Run in Production Mode:

```
python test_client.py --gateway-name external-http-gateway
```

## Tracing with Open Telemetry

This guide explains how to run the `SandboxClient` with `OpenTelemetry` tracing enabled to send traces
to `Google Cloud Trace`. This guide uses Google Cloud Trace as the observability backend. However,
because OpenTelemetry is an open-source, vendor-neutral standard, this setup can be adapted to work
with any backend that supports OTLP (OpenTelemetry Protocol).

### Prerequisites

- A Google Cloud project with the Cloud Trace, Cloud Monitoring and Logging, and Telemetry APIs enabled.
- The user or service account must have the `roles/logging.logWriter`, `roles/monitoring.metricWriter`,
  and `roles/cloudtrace.agent` permissions.
- Ensure you have `docker`, `kubectl`, and the `gcloud CLI` installed and configured.
- Follow all of the prerequisites and steps in the [sections above](#prerequisites) to create a
  cluster, install the controller, deploy the router, create a sandboxtemplate, and create a virtual
  environment with the agent-sandbox-client and the tracing dependencies installed into the .venv.

### Local Development

#### 1. Authenticate with Google Cloud

For local development, log in with Application Default Credentials. The `OpenTelemetry` collector
will use the credentials to export the traces to Google Cloud Trace.

```bash
gcloud auth application-default login
```

#### 2. Configure the Collector

Save a copy of the file named `otel-collector-config.yaml` in this directory to your current working
directory. Replace your-gcp-project-id with your actual Google Cloud project ID.

#### 3. Run the Collector

From the directory containing your `otel-collector-config.yaml` config file, run the following
command to start the collector in Docker.

```bash
docker run -d \
  --name otel-collector \
  -u "$(id -u):$(id -g)" \
  -v $(pwd)/otel-collector-config.yaml:/etc/otelcol/config.yaml \
  -v $HOME/.config/gcloud/application_default_credentials.json:/gcp/credentials.json \
  -e GOOGLE_APPLICATION_CREDENTIALS=/gcp/credentials.json \
  -p 4317:4317 \
  otel/opentelemetry-collector-contrib \
  --config /etc/otelcol/config.yaml
```

#### 4. Run the Sandbox Client with Tracing

To run the client and generate traces, instantiate the SandboxClient with the `enable_tracing=True`
flag in your Python script.

```python
from agentic_sandbox import SandboxClient

def main():
    # ...
    with SandboxClient(
        template_name="python-sandbox-template",
        enable_tracing=True
    ) as sandbox:
        # Run any client operations here
        sandbox.run("echo 'Hello, Traced World!'")

if __name__ == "__main__":
    main()
```

Alternatively, you can also run the `test_client.py` in this directory with:

```bash
python test_client.py --enable-tracing
```

#### 5. View the Traces

After running your client script, traces will be sent to Google Cloud.

- Wait a minute or two for the data to be processed.
- Go to the Google Cloud Trace Explorer.
- You will see your traces appear in the list. You can click on a trace to see the full waterfall
  diagram, including the sandbox-client.lifecycle parent span and all its children.
