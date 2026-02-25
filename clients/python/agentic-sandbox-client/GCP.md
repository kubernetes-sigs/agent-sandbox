## Tracing with Open Telemetry and Google Cloud Trace

This guide explains how to run the `SandboxClient` with `OpenTelemetry` tracing enabled to send
traces to `Google Cloud Trace`.

This guide uses Google Cloud Trace as the observability backend. However,
because OpenTelemetry is an open-source, vendor-neutral standard, this setup can be adapted to work
with any backend that supports OTLP (OpenTelemetry Protocol).

### Prerequisites

- A Google Cloud project with the Cloud Trace, Cloud Monitoring and Logging, and Telemetry APIs enabled.
- The user or service account must have the `roles/logging.logWriter`,
  `roles/monitoring.metricWriter`, and `roles/cloudtrace.agent` permissions.
- Ensure you have `docker`, `kubectl`, and the `gcloud CLI` installed and configured.
- Follow all of the prerequisites and steps in the [README](README.md#prerequisites) to create a
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

Save a copy of the file named `otel-collector-config.yaml.example` in this directory as
`otel-collector-config.yaml` in your current working directory. Replace `YOUR-GCP-PROJECT-ID` in the
file with your actual Google Cloud project ID.

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

Run `docker ps` to check that the `otel-collector` is running. If you do not see it, run
`docker logs otel-collector` to see the error message.

#### 4. Run the Sandbox Client with Tracing

To run the client and generate traces, instantiate the SandboxClient with the `enable_tracing=True`
flag in your Python script.

```python
from k8s_agent_sandbox import SandboxClient

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

## Tracing Behavior with Multiple Clients

It is generally expected that most clients within the same application process will share the same
`trace_service_name`. The default for the `SandboxClient` is `sandbox-client`.

The `SandboxClient` relies on a **process-wide singleton** `TracerProvider`. This means:

1.  **Creation**: The provider is created only **once**, upon the first initialization of a
    `SandboxClient` with tracing enabled.
2.  **Persistence**: It persists for the entire lifetime of the Python process and is shared by all
    subsequent clients.
3.  **Shutdown**: It is strictly shut down only when the process exits (via `atexit`).

Because the provider is global, the `service.name` resource attribute is **immutable** after the
first initialization. When multiple clients with different service names are used in the same
process, the **first client wins** the global service name configuration. However, each client
maintains its own **instrumentation scope**, ensuring traces remain distinguishable.

| Feature                   | Client A (Initialized First)   | Client B (Initialized Second)                     |
| :------------------------ | :----------------------------- | :------------------------------------------------ |
| **trace_service_name**    | `sandbox-client-a`             | `sandbox-client-b`                                |
| **Global `service.name`** | `sandbox-client-a` (Wins)      | `sandbox-client-a` (Ignored w/ Warning)           |
| **Instrumentation Scope** | `sandbox_client_a`             | `sandbox_client_b`                                |
| **Span Name Format**      | `sandbox-client-a.<operation>` | `sandbox-client-b.<operation>`                    |
| **Trace Backend View**    | Service: `sandbox-client-a`    | Service: `sandbox-client-a` (but separate scopes) |
