# CSN Load & Cost Tests (long-running churn)

Load tests that prove **agent-sandbox on GKE Cold Standby Nodes (CSN) + HPA
achieves the same claim latency as a statically over-provisioned cluster, at a
fraction of the node cost.**

Unlike the short ClusterLoader2 burst recipes in `../`, these run realistic
**churn** profiles (claims created _and_ expiring concurrently over tens of
minutes) so the cluster reaches a steady state and then scales down — which is
where CSN's cost advantage appears.

## What it measures

- **Latency:** `agent_sandbox_claim_controller_startup_latency_ms` P99 over time
  (claim observed → Ready). Compared static vs CSN.
- **Cost:** node-hours over the run (running at full price, suspended at ~3%),
  via `cost_graph.py`. Same machine type → node-hours ratio = cost ratio.

The thesis: `P99(CSN) ≈ P99(static)` while CSN runs at a fraction of the node
cost — savings concentrated in the steady-state and downsizing/idle phases.

## Files

| File                   | Purpose                                                                                                                                                                                                                       |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `run_churn_test.sh`    | Orchestrator: renders `manifests/` (envsubst) + applies them, runs the suspension gate, launches monitoring, drives the churn. **Start here.**                                                                                |
| `manifests/`           | The K8s objects the runner applies, as `envsubst` templates (`${VAR}`): `computeclass.yaml`, `sandbox-template.yaml`, `warmpool.yaml`, `hpa.yaml`, `asn-buffer.yaml`, `csn-buffer.yaml`. Edit these to change the test infra. |
| `churn-driver.py`      | Phase-driven claim generator. Creates claims at the profile rate and stamps `shutdownTime = now + TTL` (controller does expiry). Reports survivorship + a stats CSV.                                                          |
| `suspension-gate.sh`   | Blocks load start until N CSN nodes report `Suspended=True` (so you test _standby_, not active, capacity).                                                                                                                    |
| `monitor-all.sh`       | Launches the full monitor bundle (warmpool ready, HPA, pods, nodes, autoscaler, capacity-buffer, instance tracker) into the run folder. Auto-launched by the runner.                                                          |
| `monitor-instances.sh` | GCE instance tracker (machine type, zone, RUNNING/SUSPENDED over time).                                                                                                                                                       |
| `cost_graph.py`        | Computes node-hours + cost-efficiency % from `node-states.csv`; compares two runs or one run vs a flat baseline; optional PNG.                                                                                                |

## Prerequisites

- GKE Standard regional cluster with the standby-buffer GKE version, NAP enabled.
- **Image Streaming on** (recommended).
- Controller args: `--kube-api-qps=3000 --kube-api-burst=4000`, `--sandbox-concurrent-workers=1000`, `--sandbox-claim-concurrent-workers=1000`.
- A Custom ComputeClass pinning the machine type (`nodePoolAutoCreation: enabled`);
  the sandbox template's `nodeSelector` targets it. (The runner applies the
  ComputeClass + template + warmpool + HPA + CSN/ASN buffers itself.)
- `python3 -m pip install kubernetes` (driver); `matplotlib` for `cost_graph.py`
  PNGs; `envsubst` (gettext-base) for rendering `manifests/` — `apt-get install
gettext-base`. `kubectl` + `gcloud` configured for the cluster.

## Quick start

### CSN + ASN + HPA run (cost-optimized config)

```bash
WARMPOOL_SIZE=300 ENABLE_HPA=true HPA_MIN_REPLICAS=300 HPA_MAX_REPLICAS=1000 \
SHUTDOWN_TTL=120 \
ENABLE_CSN_BUFFER=true CSN_BUFFER_CORES=600 CSN_INIT_TIME=2m \
ENABLE_ASN_BUFFER=true ASN_BUFFER_CORES=300 \
PROFILE="5:600,30:180,0:360,5:300" \
./run_churn_test.sh csn-test
```

### Static baseline (gold standard to match)

Pre-provision a fixed fleet sized to the CSN run's _peak running_ node count,
disable autoscaling + NAP, then:

```bash
WARMPOOL_SIZE=1000 ENABLE_HPA=false \
SHUTDOWN_TTL=120 \
ENABLE_CSN_BUFFER=false ENABLE_ASN_BUFFER=false \
PROFILE="5:600,30:180,0:360,5:300" \
./run_churn_test.sh static-test
```

## Profile notation

`PROFILE="rate:duration_seconds,..."` — comma-separated segments run in order.
`"0:N"` is an idle pause (no creation) for N seconds. Population self-bounds at
`rate × SHUTDOWN_TTL`. The growth → peak → reversal → long-idle "mountain" shape
exposes both the steady-state and downsizing cost savings.

## Outputs

Each run writes to `tmp/<RUN_ID>/`:

- `churn-stats.csv` — phase, target rate, live population, created count.
- `node-states.csv` — total / suspended / resumed nodes over time (cost input).
- `warmpool_ready.csv` — spec/status/ready replicas + provisioning gap.
- `hpa.csv`, `pods_status.csv`, `nodes.log`, `capacity_buffer.log`,
  `autoscaler_events.log`, `instances-wide.csv`.
- `churn-driver.log` — includes the **survivorship report**
  (`expired_before_adopt` must be 0 for the latency metric to be trustworthy).

## Analysis

```bash
# cost efficiency: CSN vs static (node-hours ratio = cost ratio, same machine type)
python3 cost_graph.py tmp/<csn-run>/node-states.csv \
  --compare tmp/<static-run>/node-states.csv --out cost.png
```

Reads "CSN is X% of static cost → Y% cheaper." Warns if CSN ≥ static (means the
idle scale-down wasn't captured, or over-provisioning) or if the peak running
counts differ (baseline not sized fairly).

Latency: pull `agent_sandbox_claim_controller_startup_latency_ms` P99 per 1-min
window from Cloud Monitoring for both runs and overlay them.

## Health checklist for a valid run

- `node-states.csv` total **falls during the idle tail** (scale-down happened) —
  if it plateaus, cleanup lag is blocking re-suspension.
- `warmpool_ready` stays `> 0` during load (no buffer drain → no cold/un-ready
  adoptions).
- Survivorship `expired_before_adopt = 0`.
- Static baseline node count ≈ the CSN run's peak running (fair comparison).
