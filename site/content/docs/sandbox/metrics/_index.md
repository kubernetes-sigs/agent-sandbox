---
title: "Metrics"
linkTitle: "Metrics"
weight: 15
description: >
  Create a Sandbox and check its metrics.
---
## Local Observability and Telemetry Gathering

When building agentic workflows, you often need to inspect the latency, execution times, and network spans of your sandbox interactions. The `k8s_agent_sandbox` SDK integrates seamlessly with OpenTelemetry (OTel). 

Using OpenTelemetry's auto-instrumentation tools, you can surface rich metric and trace data directly in your local console for rapid debugging, all without writing any custom telemetry code or setting up external observability backends.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox controller]({{< ref "/docs/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed with tracing extras and the OpenTelemetry CLI tools: `pip install "k8s-agent-sandbox[tracing]" opentelemetry-distro`.
  > **Note:** The `[tracing]` extra automatically bundles the core `opentelemetry-api` and `opentelemetry-sdk` dependencies needed by the client. For a full list of available extras, see the [SDK dependencies documentation]({{< ref "/docs/python-client#dependencies" >}}).

### The Client Execution Workflow

The client code remains completely standard. Because we will be using OpenTelemetry's CLI wrapper to inject the instrumentation at runtime, your Python script does not need any OTel-specific imports or bootstrapping logic. 

Save the following standard execution logic to a file named `main.py`.

```python
from k8s_agent_sandbox import SandboxClient

# 1. Initialize the client
client = SandboxClient()

# 2. Create the sandbox using your template
sandbox = client.create_sandbox("simple-sandbox-template")

# 3. Define and run a standard command
payload = "echo 'Hello World!'"
response = sandbox.commands.run(payload)

# 4. Print the execution response
print(response)
```

### Bootstrapping and Execution

To actually generate and view the telemetry data, you must instruct OpenTelemetry on *where* to send the data and tell Python to load the tracing libraries. 

By setting the exporter environment variables to `console`, the data will be printed directly to your standard output. Wrapping your standard `python main.py` command with `opentelemetry-instrument` automatically bootstraps the tracing SDK.

Run the following commands in your terminal:

```bash
# 1. auto-instrument third-party libraries
opentelemetry-bootstrap -a install

# 2. Route traces and metrics to the local terminal
export OTEL_TRACES_EXPORTER="console"
export OTEL_METRICS_EXPORTER="console"

# 3. Execute the script using the OTel auto-instrumentation wrapper
opentelemetry-instrument python main.py
```

### Expected Output

When you run the command above, you will see your standard application logs (e.g., creating the claim, starting the tunnel) alongside a large payload of JSON-formatted telemetry data. 

```log
2026-04-10 11:16:01,246 - INFO - Creating SandboxClaim 'sandbox-claim-30ed22c0' in namespace 'default' using template 'simple-sandbox-template'...
2026-04-10 11:16:01,265 - INFO - Resolving sandbox name from claim 'sandbox-claim-30ed22c0'...
2026-04-10 11:16:01,284 - INFO - Resolved sandbox name 'sandbox-claim-30ed22c0' from claim status
2026-04-10 11:16:01,284 - INFO - Watching for Sandbox sandbox-claim-30ed22c0 to become ready...
2026-04-10 11:16:02,412 - INFO - Sandbox sandbox-claim-30ed22c0 is ready.
2026-04-10 11:16:02,412 - INFO - Starting tunnel for Sandbox sandbox-claim-30ed22c0
2026-04-10 11:16:02,418 - INFO - Waiting for port-forwarding to be ready...
2026-04-10 11:16:02,924 - INFO - Tunnel ready at http://127.0.0.1:52059
stdout='Hello World\n' stderr='' exit_code=0
2026-04-10 11:16:03,478 - INFO - Stopping port-forwarding for Sandbox sandbox-claim-30ed22c0...
2026-04-10 11:16:03,482 - INFO - Connection to sandbox claim 'sandbox-claim-30ed22c0' has been closed.
2026-04-10 11:16:03,497 - INFO - Terminated SandboxClaim: sandbox-claim-30ed22c0
{
    "resource_metrics": [
        {
            "resource": {
                "attributes": {
                    "telemetry.sdk.language": "python",
                    "telemetry.sdk.name": "opentelemetry",
                    "telemetry.sdk.version": "1.41.0",
                    "telemetry.auto.version": "0.62b0",
                    "service.name": "unknown_service"
                },
                "schema_url": ""
            },
            "scope_metrics": [
                {
                    "scope": {
                        "name": "opentelemetry-sdk",
                        "version": null,
                        "schema_url": "",
                        "attributes": null
                    },
                    "metrics": [
                        {
                            "name": "otel.sdk.processor.log.queue.capacity",
                            "description": "The maximum number of log records the queue of a given instance of an SDK Log Record processor can hold.",
                            "unit": "{log_record}",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "otel.component.type": "batching_log_processor",
                                            "otel.component.name": "batching_log_processor/0"
                                        },
                                        "start_time_unix_nano": 1775816160885369000,
                                        "time_unix_nano": 1775816163498738000,
                                        "value": 2048,
                                        "exemplars": []
                                    }
                                ],
                                "aggregation_temporality": 2,
                                "is_monotonic": false
                            }
                        },
                        {
                            "name": "otel.sdk.span.started",
                            "description": "The number of created spans.",
                            "unit": "{span}",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "otel.span.parent.origin": "none",
                                            "otel.span.sampling_result": "RECORD_AND_SAMPLE"
                                        },
                                        "start_time_unix_nano": 1775816163440787000,
                                        "time_unix_nano": 1775816163498738000,
                                        "value": 1,
                                        "exemplars": []
                                    }
                                ],
                                "aggregation_temporality": 2,
                                "is_monotonic": true
                            }
                        },
                        {
                            "name": "otel.sdk.span.live",
                            "description": "The number of created spans with `recording=true` for which the end operation has not been called yet.",
                            "unit": "{span}",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "otel.span.sampling_result": "RECORD_AND_SAMPLE"
                                        },
                                        "start_time_unix_nano": 1775816163440885000,
                                        "time_unix_nano": 1775816163498738000,
                                        "value": 0,
                                        "exemplars": []
                                    }
                                ],
                                "aggregation_temporality": 2,
                                "is_monotonic": false
                            }
                        },
                        {
                            "name": "otel.sdk.processor.span.queue.size",
                            "description": "The number of spans in the queue of a given instance of an SDK span processor.",
                            "unit": "{span}",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "otel.component.type": "batching_span_processor",
                                            "otel.component.name": "batching_span_processor/0"
                                        },
                                        "start_time_unix_nano": 1775816163498638000,
                                        "time_unix_nano": 1775816163498738000,
                                        "value": 1,
                                        "exemplars": []
                                    }
                                ],
                                "aggregation_temporality": 2,
                                "is_monotonic": false
                            }
                        },
                        {
                            "name": "otel.sdk.processor.log.queue.size",
                            "description": "The number of logs in the queue of a given instance of an SDK log processor.",
                            "unit": "{log}",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "otel.component.type": "batching_log_processor",
                                            "otel.component.name": "batching_log_processor/0"
                                        },
                                        "start_time_unix_nano": 1775816163498726000,
                                        "time_unix_nano": 1775816163498738000,
                                        "value": 0,
                                        "exemplars": []
                                    }
                                ],
                                "aggregation_temporality": 2,
                                "is_monotonic": false
                            }
                        }
                    ],
                    "schema_url": ""
                },
                {
                    "scope": {
                        "name": "opentelemetry.instrumentation.requests",
                        "version": "0.60b1",
                        "schema_url": "https://opentelemetry.io/schemas/1.11.0",
                        "attributes": null
                    },
                    "metrics": [
                        {
                            "name": "http.client.duration",
                            "description": "measures the duration of the outbound HTTP request",
                            "unit": "ms",
                            "data": {
                                "data_points": [
                                    {
                                        "attributes": {
                                            "http.method": "POST",
                                            "http.scheme": "http",
                                            "http.host": "127.0.0.1",
                                            "net.peer.name": "127.0.0.1",
                                            "net.peer.port": 52059,
                                            "http.status_code": 200,
                                            "http.flavor": "1.1"
                                        },
                                        "start_time_unix_nano": 1775816163477249000,
                                        "time_unix_nano": 1775816163498738000,
                                        "count": 1,
                                        "sum": 35,
                                        "bucket_counts": [
                                            0,
                                            0,
                                            0,
                                            0,
                                            1,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0,
                                            0
                                        ],
                                        "explicit_bounds": [
                                            0.0,
                                            5.0,
                                            10.0,
                                            25.0,
                                            50.0,
                                            75.0,
                                            100.0,
                                            250.0,
                                            500.0,
                                            750.0,
                                            1000.0,
                                            2500.0,
                                            5000.0,
                                            7500.0,
                                            10000.0
                                        ],
                                        "min": 35,
                                        "max": 35,
                                        "exemplars": [
                                            {
                                                "filtered_attributes": {},
                                                "value": 35,
                                                "time_unix_nano": 1775816163476643000,
                                                "span_id": 4637909099667112766,
                                                "trace_id": 27019542480527418515130145646832490510
                                            }
                                        ]
                                    }
                                ],
                                "aggregation_temporality": 2
                            }
                        }
                    ],
                    "schema_url": "https://opentelemetry.io/schemas/1.11.0"
                }
            ],
            "schema_url": ""
        }
    ]
}
{
    "name": "POST",
    "context": {
        "trace_id": "0x1453c64bd669390ef0b73508e6ef980e",
        "span_id": "0x405d29c0e235c33e",
        "trace_state": "[]"
    },
    "kind": "SpanKind.CLIENT",
    "parent_id": null,
    "start_time": "2026-04-10T10:16:03.441094Z",
    "end_time": "2026-04-10T10:16:03.477336Z",
    "status": {
        "status_code": "UNSET"
    },
    "attributes": {
        "http.method": "POST",
        "http.url": "http://127.0.0.1:52059/execute",
        "user_agent.original": "python-requests/2.33.1",
        "http.status_code": 200
    },
    "events": [],
    "links": [],
    "resource": {
        "attributes": {
            "telemetry.sdk.language": "python",
            "telemetry.sdk.name": "opentelemetry",
            "telemetry.sdk.version": "1.41.0",
            "telemetry.auto.version": "0.62b0",
            "service.name": "unknown_service"
        },
        "schema_url": ""
    }
}
```

This output will include `resource_metrics` tracking the internal latency of the client, as well as `Span` objects detailing the precise duration of the HTTP requests made to the sandbox pod.
