---
title: "Controller Metrics Reference"
linkTitle: "Controller Metrics"
weight: 34
description: >
  Prometheus metric families exposed by the Agent Sandbox controller.
---
<!-- The metric table on this page is generated into docs/metrics.md from the definitions in internal/metrics. -->
<!-- To regenerate it, run: `make generate-metrics-docs`. Everything else on this page is hand-written. -->

## Scraping the endpoint

The controller serves Prometheus metrics from `/metrics` on the address given by `--metrics-bind-address`, which defaults to `:8080`. The bundled manifests expose that port as the `metrics` port of the `agent-sandbox-controller` Service in the `agent-sandbox-system` namespace.

For a one-off local check, port-forward the Service:

```bash
kubectl -n agent-sandbox-system port-forward svc/agent-sandbox-controller 8080:8080
curl http://127.0.0.1:8080/metrics
```

For continuous collection with the Prometheus Operator:

- **Helm chart** — set `metrics.serviceMonitor.enabled=true` to create a `ServiceMonitor`, and `metrics.prometheusRule.enabled=true` to create a starter `PrometheusRule`. Both require the Prometheus Operator CRDs, and you will usually also need `metrics.serviceMonitor.additionalLabels` to match your Prometheus `serviceMonitorSelector`. See the [chart values](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/helm/README.md).
- **Plain manifests** — apply the standalone `ServiceMonitor` at [`dev/load-test/test-recipes/monitor/agent-sandbox-controller-monitor.yaml`](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/dev/load-test/test-recipes/monitor/agent-sandbox-controller-monitor.yaml), which scrapes the same port every 10 seconds.

{{% include-file file="additional/docs/metrics.md" %}}

## Availability

Only two of the families above appear in every install; the rest depend on how the controller is run.

- **Emitted by every install.** `agent_sandboxes`, recomputed on each scrape by a collector that lists every `Sandbox`, and `agent_sandbox_build_info`, a constant gauge present for the whole life of the process.
- **Require `--extensions`.** Every `SandboxClaim` family — the `agent_sandbox_claim_*` metrics and `agent_sandbox_creation_latency_ms` — is recorded by the SandboxClaim controller, which the manager only starts when it is run with `--extensions`. Without that flag they are absent entirely, not zero. `agent_sandbox_creation_latency_ms` additionally waits for the backing `Sandbox` to reach Ready.
- **Require a timestamp the controller does not write.** The end-to-end latency families measure from an annotation stamped by something upstream, and are not recorded when it is missing. `agent_sandbox_client_claim_startup_latency_ms` needs `agents.x-k8s.io/client-first-requested-at`, which the Go and Python SDKs stamp and hand-written manifests do not; a missing annotation is skipped silently, and only an unparseable one is logged, at `-v=1`. `agent_sandbox_claim_startup_latency_ms` needs `agents.x-k8s.io/webhook-first-observed-at` from the optional [timestamp-injection webhook](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/webhook-inject-timestamp/testing_webhook_guide.md), and does log the skip at `-v=1` when it is absent.

Those last two measure across a process boundary and inherit any clock skew between the client or webhook and the controller; `agent_sandbox_claim_controller_startup_latency_ms` does not.

## Label values

| Label | Values |
| --- | --- |
| `namespace` | Namespace of the `Sandbox` or `SandboxClaim`. |
| `launch_type` | `warm` when the Sandbox came from a `SandboxWarmPool`, `cold` otherwise. The claim latency histograms additionally report `unknown` when no Sandbox was resolved; `agent_sandboxes` never does, and reports a Sandbox carrying no launch-type label as `cold`. |
| `sandbox_template` | Name of the `SandboxTemplate` resolved for the object. When none can be resolved the value is `unknown` on `agent_sandboxes` and `__unknown__` on the `SandboxClaim` families. |
| `created_by` | Component that created the object, normalized to `go-client`, `python-client`, `controller`, or `unknown`. |
| `owned_by` | `SandboxClaim` or `SandboxWarmPool` when one of those controls the `Sandbox`; `None` for any other controller, or for none at all. |
| `ready_condition` | `true` or `false`, from the `Ready` condition of the `Sandbox`. |
| `expired` | `true` when the `Ready` condition reason is `SandboxExpired`, otherwise `false`. |
| `pod_condition` | `ready` when the adopted Sandbox was already Ready at adoption, `not_ready` otherwise. Despite the name this reads the Sandbox's `Ready` condition rather than the Pod's, and cold launches are always recorded as `not_ready`. |
| `warmpool_name` | The claim's `spec.warmPoolRef.name`. On a warm launch the adopted Sandbox's owning `SandboxWarmPool` is guaranteed to match it, since `verifySandboxCandidate` rejects a candidate owned by any other pool. |
| `git_version`, `git_commit`, `build_date`, `go_version`, `compiler`, `platform` | Build metadata baked into the controller binary. Constant for the lifetime of a process. |

`agent_sandbox_claim_creation_total` is incremented once per Sandbox the controller creates or adopts for a claim, not once per claim reaching Ready. The latency histograms are the guarded ones: the controller stamps `agents.x-k8s.io/claim-first-ready-at` the first time a claim goes Ready, so a later Ready to NotReady to Ready flap adds no second observation. `agent_sandboxes` needs no guard because the collector recomputes it from a live `Sandbox` list on every scrape, so its series disappear along with the sandboxes they describe, whereas every label combination the counters and histograms have observed stays in the process until it restarts.

## See Also

- [Performance Assessment]({{< ref "/docs/performance-assessment" >}}) — the load-test recipes that query `agent_sandbox_claim_startup_latency_ms` and `agent_sandbox_claim_controller_startup_latency_ms`, with the Prometheus queries and default thresholds they assert.
- [Sandbox Metrics]({{< ref "/docs/sandbox/metrics" >}}) — OpenTelemetry traces and metrics from the Python SDK, which are separate from the controller metrics on this page.
