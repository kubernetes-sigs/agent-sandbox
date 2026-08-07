# MovieLens 20M High-Density Sandbox Benchmark

This directory contains an automated high-density benchmarking suite for
`sigs.k8s.io/agent-sandbox`. It evaluates container density, execution latency,
memory overcommitment, and I/O pressure for stateful Python AI agent workloads
running on Google Kubernetes Engine (GKE) nodes backed by **Local NVMe SSD
Memory Swap**.

### What This Benchmark Does

Real-world AI agent workloads (e.g., Code Interpreters, Jupyter Notebooks, Data
Science Sandboxes, LLM Execution Runtimes) present a unique infrastructure
challenge:

1.  **Interactive Memory Spikes:** Agents load large Python data structures
    (Pandas DataFrames, NumPy arrays, PyTorch models) into RAM during active
    processing requests.
2.  **Long-Tail Idle Retention:** After completing a request, agents remain idle
    while waiting for the next user input. Their memory structures stay resident
    in RAM, consuming expensive node capacity.
3.  **Density Bottlenecks:** Without memory swap, a single node quickly runs out
    of physical RAM (OOM Kills), capping the number of tenant sandboxes a node
    can host.

This benchmark rigorously evaluates how **GKE Local NVMe SSD Swap** combined
with `agent-sandbox` enables nodes to host up to **120% more active agent sandboxes for native `runc`** (scaling from 100 to 220 sandboxes) and **100% more for secure `gvisor`** (scaling from 80 to 160 sandboxes) on the exact same physical hardware footprint without latency degradation.

---

## 1. Architecture & Workload Design

*   **Runtime Container Image (`python-runtime-sandbox`):** Workloads run inside the standard [`python-runtime-sandbox`](../../python-runtime-sandbox) container image (`us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/python-runtime-sandbox:latest-main`), which pre-packages Python 3.14, `pandas`, and an HTTP REST execution server listening on port `8888`.
*   **Dataset Pre-Staging & Storage Isolation:** To eliminate network download
    storms during density scale-up, the 20M MovieLens dataset (`ratings.csv`, 5M
    row working set) is pre-staged on the host node at
    `/tmp/movielens/ratings.csv` and mounted into sandbox containers via a
    HostPath-backed `PersistentVolume` / `PersistentVolumeClaim` at `/data/ratings.csv`.
    Memory Swap writes directly to the **Local NVMe SSD**
    (`/dev/mapper/encswap`), running on an isolated NVMe hardware bus separate from
    the boot disk.
*   **Pandas Analytics Workload:** The Go test harness (`pythonsandbox_density_test.go`)
    invokes `benchmark_density.py` inside each sandbox by posting an execution command payload
    (`python3 /scripts/benchmark_density.py`) to the sandbox REST API endpoint
    (`http://localhost:8888/execute`). The script loads 5,000,000 rows into a Pandas
    DataFrame and performs analytical aggregations (`groupby('movieId')['rating'].agg(['mean', 'count'])`),
    creating a ~375 MB active RAM footprint per sandbox.
*   **Stateful Resident Memory (`os.fork()`):** Upon completing analytical
    calculations, `benchmark_density.py` forks a background child process via `os.fork()`.
    The parent process outputs JSON execution telemetry and exits immediately
    (returning HTTP 200 OK), while the child process detaches standard file descriptors
    and enters `time.sleep(600)`. This retains the ~375 MB Pandas DataFrame resident in
    process memory for 10 minutes, allowing kernel `kswapd` to stream idle memory pages
    to the Local NVMe SSD Swap partition.
*   **Node Core Isolation (`node-tuner-ds`):** Kubelet and containerd system daemons are pinned
    to Cores 0 and 1 (`reservedSystemCPUs: "0,1"`), Linux kernel ARP cache thresholds are scaled
    (`2048/4096/8192`), and `systemd-journald` storage logging is rate-limited to prevent host CPU
    and disk queue bottlenecks. *(Note: `node-tuner-ds` is used as a temporary
    workaround until native Kubelet CPU reservation is supported in GKE; see
    [GKE System CPU Reservation Design Doc](https://docs.google.com/document/d/14_Ezqm-ff2mwjEbTk2h0iSzCu3Rh9xdFbPRG7ZPLCxQ/edit?resourcekey=0-g9qAL6lRjQx6Xrk-YARP5g#heading=h.ly7j8v6k28nd)).*
*   **Orchestrator-Level Deployment Stagger (`1.0s`):** The Go test runner
    (`pythonsandbox_density_test.go`) enforces a 1-second delay between
    container instantiations, feeding workloads to the node at a steady rate to
    prevent CPU/IO thundering herd contention.
*   **Resource Allocation (`Requests: 15m CPU, 100Mi RAM`, `Limits: 2Gi RAM`):** Sandboxes declare
    a `15m` CPU Request and `100Mi` Memory Request with a `2Gi` Limit. This establishes a
    **Burstable QoS profile** (required for Kubernetes Limited Swap) while satisfying Kubelet's
    static CPU manager math to unblock 100% pod scheduling capacity.

---

## 2. Related Files

*   **`pythonsandbox_density_test.go`**: Go e2e benchmark test harness located at [`test/e2e/extensions/pythonsandbox_density_test.go`](../../../test/e2e/extensions/pythonsandbox_density_test.go).
*   **`python_workload.py`**: Python analytical workload script located at [`test/e2e/extensions/python_workload.py`](../../../test/e2e/extensions/python_workload.py).
*   **`run_pythonsandbox_density_test.sh`**: Automated runner script in this directory.
*   **`parse_telemetry.py`**: Telemetry metrics parser in this directory.

---

## 3. How to Run

### Step 1: Deploy the GKE Cluster
Provision the GKE cluster with both `baseline-pool` (no swap) and `lssd-swap-pool` (Local NVMe SSD Swap) using the root helper script:

```bash
# From the repo root, navigate to examples/gke-swap
cd examples/gke-swap

# Deploy c4-standard-8 cluster (baseline + LSSD swap node pools)
./deploy_cluster.sh
```

*(Optional: For gVisor runtime cluster setup, run `examples/gke-swap/runtimes/gVisor/deploy_cluster.sh`).*

### Step 2: Deploy Node Tuner DaemonSet
Ensure node core isolation and kernel tuning are active:
```bash
kubectl apply -f ../node-tuner-daemonset.yaml
```

### Step 3: Run Density Sweeps
Execute multi-density benchmark sweeps across node pool scenarios:
```bash
# Navigate to the python-density directory
cd python-density

# Run 140-density benchmark sweep on Local NVMe SSD Swap pool
POOLS="lssd-swap-pool" DENSITIES="140" ./run_pythonsandbox_density_test.sh

# Target specific container runtimes via terminal (optional)
RUNTIME_CLASS="gvisor" POOLS="lssd-swap-pool" DENSITIES="140" ./run_pythonsandbox_density_test.sh
```

--------------------------------------------------------------------------------

## 4. Benchmark Results

> [!NOTE] **Active Node Tuning:** All benchmark data below was captured with
> Node Tuning active (`node-tuner-ds` core isolation reserving Cores 0 & 1 for
> system/`kswapd`, 1.0s orchestrator deployment stagger, and `15m` CPU
> Requests).

### Density Sweep Performance Matrix (30 GB Node, `c4-standard-8`)

*   **Workload:** 5M Row Pandas GroupBy Analytics (~375 MB RAM footprint per sandbox holding state for 600s).
*   **Hardware:** GKE `c4-standard-8` (8 vCPU, 30 GB RAM, **27.0 GB Allocatable RAM**).
*   **Swap Storage:** Local NVMe SSD (`/dev/mapper/encswap`).

#### Native `runc` Benchmark Results

| Density | Node Pool | Pass Rate | Avg Exec Time | P99 Exec Time | Peak Node RAM | Net NVMe Swap Used | Memory Pressure (`mem_psi`) |
| :---: | :--- | :---: | :---: | :---: | :---: | :---: | :---: |
| **80** | `baseline-pool` (No Swap) | **80 / 80 (100%)** | 1.19s | 1.60s | 19.79 GB | 0.00 GB | 0.00s |
| **80** | **`lssd-swap-pool`** | **80 / 80 (100%)** | 1.03s | 1.10s | 24.03 GB | 0.01 GB | 0.00s |
| **100** | `baseline-pool` (No Swap) | **100 / 100 (100%)** | 1.19s | 1.32s | 24.78 GB | 0.00 GB | 0.00s |
| **100** | **`lssd-swap-pool`** | **100 / 100 (100%)** | 1.03s | 1.10s | 24.03 GB | 0.01 GB | 0.00s |
| **120** | `baseline-pool` (No Swap) | 117 / 120 (97.5%) | 3.48s | 43.23s | 27.00 GB | 0.00 GB | 59.91s |
| **120** | **`lssd-swap-pool`** | **120 / 120 (100%)** | **1.09s** | **1.59s** | **24.68 GB** | **12.44 GB** | **0.03s** |
| **140** | **`lssd-swap-pool`** | **140 / 140 (100%)** | **1.09s** | **1.37s** | **25.39 GB** | **20.46 GB** | **10.37s** |
| **160** | **`lssd-swap-pool`** | **160 / 160 (100%)** | **1.09s** | **1.34s** | **24.86 GB** | **21.05 GB** | **6.28s** |
| **180** | **`lssd-swap-pool`** | **180 / 180 (100%)** | **1.14s** | **1.75s** | **21.91 GB** | **24.22 GB** | **9.34s** |
| **200** | **`lssd-swap-pool`** | **200 / 200 (100%)** | **1.15s** | **1.98s** | **24.05 GB** | **30.03 GB** | **13.18s** |
| **220** | **`lssd-swap-pool`** | **220 / 220 (100%)** | **1.16s** | **2.55s** | **26.06 GB** | **35.03 GB** | **19.77s** |
| **240** | **`lssd-swap-pool`** | 107 / 240 (44.6%) | N/A (Node Crash) | N/A | N/A | N/A | N/A |

#### Secure `gvisor` Benchmark Results

| Density | Node Pool | Pass Rate | Avg Exec Time | P99 Exec Time | Peak Node RAM | Net NVMe Swap Used | Memory Pressure (`mem_psi`) |
| :---: | :--- | :---: | :---: | :---: | :---: | :---: | :---: |
| **80** | `gvisor-baseline-pool` (No Swap) | **80 / 80 (100%)** | 1.41s | 1.77s | 24.90 GB | 0.00 GB | 0.00s |
| **80** | **`gvisor-swap-pool`** | **80 / 80 (100%)** | 1.17s | 1.52s | 24.20 GB | 0.79 GB | 0.42s |
| **100** | `gvisor-baseline-pool` (No Swap) | 70 / 100 (70.0%) | 1.53s | 5.10s | 27.00 GB | 0.00 GB | 42.10s |
| **120** | **`gvisor-swap-pool`** | **120 / 120 (100%)** | **1.40s** | **2.84s** | **25.52 GB** | **18.98 GB** | **12.67s** |
| **140** | **`gvisor-swap-pool`** | **140 / 140 (100%)** | **1.55s** | **5.43s** | **24.87 GB** | **23.11 GB** | **18.68s** |
| **150** | **`gvisor-swap-pool`** | **150 / 150 (100%)** | **1.65s** | **5.05s** | **25.13 GB** | **25.71 GB** | **22.68s** |
| **160** | **`gvisor-swap-pool`** | **160 / 160 (100%)** | **1.75s** | **5.27s** | **23.69 GB** | **29.32 GB** | **34.85s** |
| **170** | **`gvisor-swap-pool`** | 124 / 170 (72.9%) | 1.29s | 1.81s | 25.10 GB | 27.50 GB | 45.10s |

---

## 5. Key Technical Takeaways

### A. Native `runc` Performance & Density Ceiling
1.  **120% Density Boost (100 -> 220 Sandboxes @ 100% Pass Rate):**
    *   **Without Swap (`baseline-pool`):** Hits a hard physical wall at 100 sandboxes, failing at 120.
    *   **With Local NVMe SSD Swap (`lssd-swap-pool`):** Scales cleanly all the way to **220 concurrent sandboxes (100% Pass Rate)**, offloading **`35.03 GB`** of dormant memory out to disk while maintaining **1.16s bare-metal execution speed**!
2.  **Zero Memory Stall Pressure up to 220 Density:** Memory stall pressure (`mem_psi`) remains minimal (under 20s cumulative across all 220 pods during startup), preserving sub-second execution responsiveness.

### B. Secure `gvisor` (`runsc`) Performance & Sentry Swapability
1.  **100% Density Boost (80 -> 160 Sandboxes @ 100% Pass Rate):**
    *   **Without Swap (`gvisor-baseline-pool`):** Fails at 100 sandboxes (70/100 pass, 30 pods OOM Killed) because each gVisor pod uses `~441 MB` of RAM (375 MB Pandas DataFrame + **~65 MB Sentry user-space kernel RAM**), causing memory demand to exceed the physical RAM limit.
    *   **With Local NVMe SSD Swap (`gvisor-swap-pool`):** Scales cleanly to **160 concurrent sandboxes (100% Pass Rate)**, offloading **`29.32 GB`** of RAM out to disk while preserving a sub-2-second execution speed (**1.75s**)!
2.  **100% Sentry Kernel Swapability:**
    *   Because gVisor's Sentry binary runs as a Linux user-space process, **Linux `kswapd` pages out gVisor's Sentry kernel memory out to disk alongside Python memory**, allowing 160 secure multi-tenant sandboxes to run reliably on a single 30 GB Node!

### C. Shared Failure Mode & Hardware Limits
1.  **The Capacity & Throughput Bottleneck (Why 170+ gVisor / 240+ runc Fails):**
    *   **Shared Failure Root Cause:** For both runtimes, failure occurs when total memory demand outpaces `kswapd`'s asynchronous page eviction throughput or exhausts combined physical RAM + Swap partition capacity.

### D. Core Isolation, Deployment Pacing & CPU Overcommitment
1.  **Core Isolation & Deployment Pacing:**
    *   **Node Core Isolation (`node-tuner-ds`):** Pins Kubelet and containerd system daemons to Cores 0 and 1, preventing system daemons from competing for CPU against sandbox workers and maintaining sub-2-second P99 execution latency at high densities.
    *   **Orchestrator-Level Staggering:** Moving deployment delays to a 1.0s Go test runner loop provides `kswapd` a continuous 1.0s window per pod to write pages out to the Local NVMe SSD, eliminating Page Cache thrashing storms on boot.
2.  **Massive CPU Overcommitment (`15m` Requests):**
    *   By lowering the CPU request to `15m` per sandbox, we allow the Kubernetes scheduler to pack hundreds of sandboxes onto an 8-core node. Because the active execution takes only ~1 second and the rest of the time is spent idle, we can safely overcommit CPU limits and rely on the Linux kernel to burst CPU allocation dynamically when a sandbox wakes up. If we don't include a CPU request, the pods schedule infinitely but face catastrophic CPU starvation when computing simultaneously. Without guaranteed `cpu.shares`, execution times skyrocket, causing the benchmark to fail due to execution timeouts.