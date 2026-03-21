from prometheus_client import Histogram

DISCOVERY_LATENCY_MS = Histogram(
    "sandbox_client_discovery_latency_ms",
    "Total time in Gateway IP assignment or kubectl port-forward setup.",
    ["status", "mode"],
    buckets=[100, 500, 1000, 5000, 10000, 30000, 60000]
)
