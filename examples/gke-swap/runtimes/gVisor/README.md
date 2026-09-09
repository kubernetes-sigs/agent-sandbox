# High-Density Agent Sandbox under gVisor on GKE with Local SSD Swap

> **Note**: For a high-level comparison across all sandboxed container runtimes (`gVisor`, `kata-qemu`, `kata-clh`), see the [**Runtime Comparison Overview**](../README.md).

This directory contains configuration, deployment instructions, and performance benchmark findings for running **gVisor (`runsc`)** isolated containers under high density (60–180 sandboxes) on a single `c4-standard-8` (30 GB RAM, 8 vCPU) GKE instance.

By combining GKE Memory Swap on dedicated Local SSDs with node-level CPU pinning and system log rate-limiting, gVisor-isolated Chrome sandboxes can scale up to **160 concurrent instances** with 100% success (with 180 pods representing the attempted failure threshold).

---

## 1. Host Memory Footprint & Page Cache Sharing

Process-level physical RAM (Resident Set Size - RSS) and incremental host RSS measured across sequential pod deployments on a clean cluster:

| Component | Host RSS | Role & Description |
| :--- | :--- | :--- |
| **gVisor Sentry** (`runsc-sandbox`) | **~311.0 MiB** | Emulated user-space kernel mapping the Chromium process tree. |
| **gVisor Gofer** (`runsc-gofer`) | **~25.0 MiB** | Guest-to-host filesystem translation proxy. |
| **Control Shim** | **~34.0 MiB** | Containerd gVisor runtime supervisor. |
| **Total Resident Footprint** | **~370.0 MiB** | **Per-pod startup host RSS.** |
| **Incremental Host RSS** | **~206.0 MiB** | **Additional physical host RSS consumed per pod.** |

---

## 2. Performance Results (gVisor Density Matrix)

All benchmark runs below were conducted on `c4-standard-8` node pools with node tuning applied.

### 2.1. Baseline Pool (No Swap)
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **baseline-pool** | 60 | 4.24s / 8.92s | 11.66s / 26.10s | 10.07 GB | 25.45% | 0.00% / 0.00% | 4.53% / 0.41% | **SUCCESS** |
| **baseline-pool** | 80 | 6.40s / 20.90s | 17.59s / 41.07s | 23.15 GB | 73.50% | 0.01% / 0.00% | 31.65% / 0.90% | **SUCCESS** |
| **baseline-pool** | **100** | — / — | — / — | N/A | 55.53% | 61.35% / 23.42% | 78.75% / 19.34% | **FAILED (93.0% Success)** |

### 2.2. Local SSD Swap Pool (Tuned)
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | Peak Host Swap | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **lssd-swap-pool** | 60 | 4.51s / 11.43s | 9.84s / 22.10s | 16.81 GB | 0.00 GB | 71.64% | 0.03% / 0.00% | 13.51% / 1.37% | **SUCCESS** |
| **lssd-swap-pool** | 80 | 5.03s / 12.63s | 13.41s / 30.62s | 21.98 GB | 0.23 GB | 67.61% | 1.70% / 0.35% | 24.83% / 1.08% | **SUCCESS** |
| **lssd-swap-pool** | 100 | 8.82s / 41.81s | 19.99s / 76.11s | 23.63 GB | 7.30 GB | 88.16% | 23.66% / 1.74% | 49.83% / 0.80% | **SUCCESS** |
| **lssd-swap-pool** | 120 | 13.79s / 50.11s | 31.31s / 97.40s | 21.56 GB | 11.60 GB | 78.69% | 29.66% / 2.17% | 38.22% / 1.21% | **SUCCESS** |
| **lssd-swap-pool** | 140 | 20.98s / 65.20s | 48.29s / 149.32s | 21.77 GB | 19.20 GB | 85.20% | 46.76% / 3.54% | 52.77% / 1.09% | **SUCCESS** |
| **lssd-swap-pool** | **160** | 27.13s / 101.54s | **59.02s / 214.95s** | 21.75 GB | **17.29 GB** | 89.24% | 63.27% / 5.76% | 62.25% / 0.85% | **SUCCESS** |
| **lssd-swap-pool** | **180** | — / — | — / — | 18.20 GB | **25.51 GB** | 93.12% | 82.05% / 7.60% | 78.39% / 2.04% | **FAILED (85.56% Success)** |

---

## 3. Key Takeaways & Impact of Local SSD Swap

### 3.1. Effect of Local SSD Swap on gVisor (+100% Density Gain)
- **Doubled Maximum Stable Density (80 → 160 Pods)**: Without swap, gVisor Sentry user-space kernels exhaust host physical RAM past 80 pods. Dedicated Local SSD Swap absorbs cold anonymous memory heaps from idle Chrome processes, enabling gVisor to scale reliably to **160 pods with 100% success**.

### 3.2. CPU Pinning & Kubelet Protection (Cores 0–1)
On untuned nodes, emulating syscalls for 180 Chrome instances consumes high host CPU across all cores. Emulation threads starve Kubelet of CPU time, causing missed heartbeats and leading GKE to flag the node `NotReady`.

Applying the node tuning DaemonSet enforces:
- `--reserved-cpus=0,1` and `--cpu-manager-policy=static` on Kubelet.
- Linux cgroups reserving Cores 0 and 1 strictly for system services and Kubelet.
- Workload containers execute on Cores 2–7, preventing host daemon CPU starvation.
- Log storage for `systemd-journald` rate-limited (`RateLimitIntervalSec=30s`, `RateLimitBurst=1000`).

Node tuning reduces overall Sandbox Ready P99 latency by **~60%** and Chrome Ready P99 latency by **~40%**, expanding maximum stable density from 140 (untuned) to **160 pods (tuned)**.

---

## How to Run the gVisor Density Benchmark

### Step 1: Provision Cluster

Deploy a cluster with gVisor enabled on both `baseline-pool` and `lssd-swap-pool`:

```bash
chmod +x deploy_cluster.sh
./deploy_cluster.sh
```

### Step 2: Apply Node Tuner DaemonSet

Apply host CPU pinning and system log rate limiting:

```bash
export KUBECONFIG="$(git rev-parse --show-toplevel)/bin/KUBECONFIG"
kubectl apply -f ../../node-tuner-daemonset.yaml
kubectl rollout status daemonset/node-tuner-ds -n kube-system
```

### Step 3: Execute Benchmark

Target the `gvisor` runtime class and execute the density sweep:

```bash
export RUNTIME_CLASS="gvisor"
export SCENARIOS="lssd-swap-pool"
export DENSITIES="60 80 100 120 140 160 180"

../../run_chromesandbox_density_test.sh
```

### Step 4: Analyze Metrics
Raw metric JSON files are saved to `artifacts/lssd-swap-pool/<density>/TestChromeSandboxDensity/density_metrics.json`.
