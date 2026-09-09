# High-Density Agent Sandbox under Kata Containers on GKE with Local SSD Swap

> **Note**: For a high-level comparison across all sandboxed container runtimes (`gVisor`, `kata-qemu`, `kata-clh`), see the [**Runtime Comparison Overview**](../README.md).

This directory contains configuration, deployment instructions, and performance benchmark findings for running **Kata Containers** isolated MicroVM sandboxes on Google Kubernetes Engine (GKE).

We evaluate two VMM (Virtual Machine Monitor) backends supported by Kata Containers on GKE:
1. **`kata-qemu`**: Hardware-virtualized MicroVMs using the standard QEMU hypervisor.
2. **`kata-clh`**: Hardware-virtualized MicroVMs using the Rust-based Cloud Hypervisor VMM.

---

## 1. VMM Architectural Comparison: QEMU vs. Cloud Hypervisor

| Dimension / Feature | Kata QEMU (`kata-qemu`) | Kata Cloud Hypervisor (`kata-clh`) |
| :--- | :--- | :--- |
| **VMM Architecture** | Legacy monolithic C hypervisor | Async-first Rust VMM |
| **VMM Config Payload** | Synchronous serial vCPU hotplugging (seconds) | Single HTTP PUT payload (milliseconds) |
| **Thread Synchronization** | Big QEMU Lock (BQL) monolithic mutex | Decoupled VMM control channel & guest vCPU threads |
| **Max Stable Density (No Swap)** | **20 Pods** | **40 Pods** |
| **Max Stable Density (LSSD Swap)**| **20 Pods** | **50 Pods** |

---

## 2. Host Memory Footprint per Pod

Memory footprints were collected via process-level RSS (`/proc/<pid>/status`) and sequential pod step-up measurements on a `c4-standard-8` (30 GB RAM, 8 vCPU) instance:

| Component | `kata-qemu` Host RSS | `kata-clh` Host RSS | Description / Function |
| :--- | :--- | :--- | :--- |
| **MicroVM Hypervisor** | **~678.0 MiB** (`qemu`) | **~664.7 MiB** (`cloud-hypervisor`) | Non-shareable VMM process holding guest kernel & Chrome heap. |
| **`virtiofsd`** | **~421.0 MiB** | **~426.8 MiB** | Filesystem translation proxy. Host Page Cache deduplicates shared binary pages. |
| **Control Shim** | **~35.0 MiB** | **~36.0 MiB** | Containerd Kata runtime supervisor. |
| **Total Resident Footprint** | **~1,130 MiB** | **~1,128 MiB** | Cumulative host RSS per pod. |
| **Incremental Host RSS** | **~631.5 MiB** | **~687.5 MiB** | Additional physical host RSS consumed per pod. |

---

## 3. Performance Results & Density Sweeps

All benchmark runs were conducted on `c4-standard-8` node pools with node tuning applied.

### 3.1. Kata QEMU (`kata-qemu`) Density Matrix

#### Baseline Pool (No Swap)
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **baseline-pool** | 20 | 4.86s / 8.40s | 16.44s / 24.92s | 10.72 GB | 53.59% | ~0% / ~0% | 10.14% / 2.44% | **SUCCESS** |
| **baseline-pool** | **30** | — / — | — / — | 17.58 GB | 68.69% | 0.21% / 0.14% | 10.38% / 3.27% | **FAILED (83.3% Success)** |

#### Local SSD Swap Pool
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | Peak Host Swap | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **lssd-swap-pool** | 20 | 4.95s / 7.66s | 16.16s / 24.28s | 3.61 GB | 0.03 GB | — | — | — | **SUCCESS** |
| **lssd-swap-pool** | **30** | — / — | — / — | 14.96 GB | 2.49 GB | 63.50% | 2.13% / 1.17% | 16.95% / 3.18% | **FAILED (86.7% Success)** |

---

### 3.2. Kata Cloud Hypervisor (`kata-clh`) Density Matrix

#### Baseline Pool (No Swap)
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **baseline-pool** | 30 | 7.00s / 12.90s | 22.11s / 35.59s | 17.92 GB | 75.49% | 0.76% / 0.33% | 14.73% / 1.28% | **SUCCESS** |
| **baseline-pool** | **40** | 10.76s / 21.85s | 34.54s / 60.99s | 26.45 GB | 75.66% | 1.73% / 0.60% | 20.27% / 5.72% | **SUCCESS** |
| **baseline-pool** | 50 | — / — | — / — | 26.45 GB | 73.13% | 1.27% / 0.44% | 21.93% / 7.52% | **FAILED (80% Success)** |

#### Local SSD Swap Pool
| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Peak Host RAM | Peak Host Swap | CPU PSI Avg | Mem PSI Avg | IO PSI Avg | Test Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **lssd-swap-pool** | 30 | 6.79s / 14.04s | 21.10s / 36.71s | 11.09 GB | 0.01 GB | 19.80% | 0.26% / 0.10% | 3.68% / 1.45% | **SUCCESS** |
| **lssd-swap-pool** | 40 | 11.62s / 24.32s | 41.50s / 85.32s | 29.14 GB | 3.47 GB | 38.96% | 5.97% / 1.49% | 10.08% / 3.63% | **SUCCESS** |
| **lssd-swap-pool** | **50** | 17.81s / 55.63s | **71.02s / 174.14s** | 29.14 GB | **12.31 GB** | 40.07% | 5.42% / 1.34% | 11.38% / 4.55% | **SUCCESS** |
| **lssd-swap-pool** | 60 | — / — | — / — | 23.04 GB | 13.08 GB | 94.57% | 15.70% / 2.59% | 13.96% / 0.08% | **FAILED (81.0% Success)** |

---

## 4. Key Takeaways & Failure Modes Analysis

### 1. Effect of Local SSD Swap on Kata MicroVMs
- **Kata Cloud Hypervisor (`kata-clh`)**: Local SSD Swap expands stable pod density from **40 pods to 50 pods (+25% density gain)** with 100% success. Cloud Hypervisor's async Rust architecture decouples VMM communications from vCPUs, allowing swap to absorb memory overcommit safely. At 60 pods, physical CPU cores become the primary bottleneck.
- **Kata QEMU (`kata-qemu`)**: Local SSD Swap **provides zero density gain** (stuck at 20 pods) and introduces a new failure mode. When host OS pages VM blocks to NVMe swap, guest page faults block host threads in D-state. In QEMU, the legacy monolithic Big QEMU Lock (BQL) causes vCPU threads in D-state to lock up the main VMM event loop, triggering containerd-shim keep-alive timeouts (15s).

### 2. Why `kata-clh` Outperforms `kata-qemu` (40 vs 20 Baseline, 50 vs 20 Swap)
- **VMM Boot Payload**: `kata-qemu` hotplugs vCPUs one by one (synchronous, taking seconds per pod), whereas `kata-clh` sends a single HTTP PUT request containing the VM configuration payload, completing in milliseconds.
- **BQL vs Async VMM Architecture**: Cloud Hypervisor's async Rust VMM architecture reduces D-state stalls to 0.069% (13x lower than QEMU), avoiding control channel deadlocks under memory pressure.

### 3. Node Boot Disk Sizing Requirement (≥150–250 GB)
Headless Chrome allocates a ~2 GB sparse cache file in `/tmp` (which maps via Virtio-FS to host OverlayFS). Across 40+ sandboxes, this generates 80 GB+ of logical write pressure. Combined with the base node OS image and container layers (~15–20 GB), a default 100 GB boot disk hits 95–100% capacity. This trips Kubelet `DiskPressure` evictions and crashes the host-side `virtiofsd` daemon with `SIGBUS` (exit code 135). 

To ensure Kubelet never triggers `DiskPressure` GC during high-density boot storms, node boot disks should be provisioned with adequate headroom (**≥150–250 GB**; 250 GB was used in the benchmark study).

### 4. CPU Core Exhaustion at 60 Pods (`kata-clh`)
With Local SSD Swap enabled and Kubelet protected by CPU isolation on Cores 0–1, `kata-clh` reaches **50 pods with 100% success**. At 60 pods, the remaining 6 physical workload cores are completely exhausted by 60 concurrent VMM hypervisor loops and pageout tasks. Guest boot times stretch past 3 minutes, causing timeouts in 19% of sandboxes.

---

## How to Run the Kata Density Benchmark

### Step 1: Provision Cluster & Register RuntimeClasses

The provided [`deploy_cluster.sh`](deploy_cluster.sh) script automatically creates nested-virtualization node pools and registers both `kata-qemu` and `kata-clh` RuntimeClasses:

```bash
chmod +x deploy_cluster.sh
./deploy_cluster.sh
```

### Step 2: Apply Node Tuner DaemonSet

Apply host CPU isolation and system log rate limiting:

```bash
export KUBECONFIG="$(git rev-parse --show-toplevel)/bin/KUBECONFIG"
kubectl apply -f ../../node-tuner-daemonset.yaml
kubectl rollout status daemonset/node-tuner-ds -n kube-system
```

### Step 3: Execute Density Sweep

Target either `kata-qemu` or `kata-clh` by setting `RUNTIME_CLASS`:

```bash
# Run Cloud Hypervisor benchmark sweep (up to 50 density)
export RUNTIME_CLASS="kata-clh"
export SCENARIOS="lssd-swap-pool"
export DENSITIES="20 30 40 50"

../../run_chromesandbox_density_test.sh
```

### Step 4: Verify MicroVM Isolation

Verify that sandboxes are assigned a Kata RuntimeClass and executing inside an isolated guest kernel:

```bash
POD_NAME=$(kubectl get pods -n perf-chromesandbox -l app=chrome-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl get pod $POD_NAME -n perf-chromesandbox -o jsonpath='{.spec.runtimeClassName}'
# Output: kata-clh (or kata-qemu)

kubectl exec -it $POD_NAME -n perf-chromesandbox -- uname -r
```
Checking `spec.runtimeClassName` verifies Kata MicroVM scheduling, and `uname -r` confirms the guest kernel version running inside the isolated sandbox.
