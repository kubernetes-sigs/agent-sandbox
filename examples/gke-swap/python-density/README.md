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
with `agent-sandbox` enables nodes to host up to **200% more active agent sandboxes for native `runc`** (scaling from 80 to 240 sandboxes) and **200% more for secure `gvisor`** (scaling from 80 to 240 sandboxes) on the exact same physical hardware footprint.

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
*   **Orchestrator-Level Deployment Stagger (`1.8s`):** The Go test runner
    (`pythonsandbox_density_test.go`) enforces a 1.8-second delay between
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
> system/`kswapd`, 1.8s orchestrator deployment stagger, and `15m` CPU
> Requests).

### Density Sweep Performance Matrix (30 GB Node, `c4-standard-8`)

*   **Workload:** 5M Row Pandas GroupBy Analytics (~375 MB RAM footprint per sandbox holding state for 600s).
*   **Hardware:** GKE `c4-standard-8` (8 vCPU, 30 GB RAM, **27.0 GB Allocatable RAM**).
*   **Swap Storage:** Local NVMe SSD (`/dev/mapper/encswap`).

#### Native `runc` Benchmark Results

| Density | Node Pool | Pass Rate | Avg Exec Time | P99 Exec Time | Peak Node RAM | Net NVMe Swap Used | Memory PSI (`mem_psi`) | I/O PSI (`io_psi`) | CPU PSI (`cpu_psi`) |
| :---: | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **60** | `baseline-pool` (No Swap) | **60 / 60 (100%)** | 1.58s | 1.79s | 15.94 GB | 0.00 GB | 0.00s | 1.66s | 12.17s |
| **80** | `baseline-pool` (No Swap) | **80 / 80 (100%)** | 1.62s | 1.86s | 19.58 GB | 0.00 GB | 0.00s | 2.30s | 15.85s |
| **100** | `baseline-pool` (No Swap) | **Failed (Node NotReady)** | 1.63s | 2.15s | 20.58 GB | 0.00 GB | 336.44s | 445.79s | 31.27s |
| **60** | **`lssd-swap-pool`** | **60 / 60 (100%)** | **1.36s** | **1.47s** | **16.52 GB** | **0.00 GB** | **0.00s** | **2.44s** | **6.12s** |
| **80** | **`lssd-swap-pool`** | **80 / 80 (100%)** | **1.31s** | **1.43s** | **20.80 GB** | **2.29 GB** | **1.06s** | **4.52s** | **6.70s** |
| **100** | **`lssd-swap-pool`** | **100 / 100 (100%)** | **1.31s** | **1.59s** | **19.88 GB** | **11.00 GB** | **1.46s** | **10.56s** | **4.43s** |
| **120** | **`lssd-swap-pool`** | **120 / 120 (100%)** | **1.33s** | **1.62s** | **19.36 GB** | **15.97 GB** | **3.26s** | **18.77s** | **13.97s** |
| **140** | **`lssd-swap-pool`** | **140 / 140 (100%)** | **1.35s** | **1.67s** | **18.86 GB** | **19.61 GB** | **5.23s** | **19.36s** | **16.24s** |
| **160** | **`lssd-swap-pool`** | **160 / 160 (100%)** | **1.40s** | **2.03s** | **20.48 GB** | **21.96 GB** | **9.33s** | **29.47s** | **20.82s** |
| **180** | **`lssd-swap-pool`** | **180 / 180 (100%)** | **1.36s** | **1.85s** | **21.03 GB** | **29.45 GB** | **8.59s** | **31.16s** | **21.26s** |
| **200** | **`lssd-swap-pool`** | **200 / 200 (100%)** | **1.43s** | **2.00s** | **23.68 GB** | **32.67 GB** | **13.21s** | **36.50s** | **31.63s** |
| **220** | **`lssd-swap-pool`** | **220 / 220 (100%)** | **1.40s** | **1.76s** | **21.44 GB** | **37.77 GB** | **9.80s** | **42.87s** | **38.78s** |
| **240** | **`lssd-swap-pool`** | **240 / 240 (100%)** | **1.39s** | **1.63s** | **18.08 GB** | **42.79 GB** | **13.60s** | **46.17s** | **42.05s** |


#### Secure `gvisor` Benchmark Results

| Density | Node Pool | Pass Rate | Avg Exec Time | P99 Exec Time | Peak Node RAM | Net NVMe Swap Used | Memory PSI (`mem_psi`) | I/O PSI (`io_psi`) | CPU PSI (`cpu_psi`) |
| :---: | :--- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **60** | **`gvisor-baseline-pool`** | **60 / 60 (100%)** | **1.94s** | **25.56s** | **17.57 GB** | **0.00 GB** | **0.00s** | **0.81s** | **12.89s** |
| **80** | **`gvisor-baseline-pool`** | **80 / 80 (100%)** | **2.04s** | **4.97s** | **26.02 GB** | **0.00 GB** | **3.90s** | **13.75s** | **38.45s** |
| **100** | **`gvisor-baseline-pool`** | **Failed (Node NotReady)** | **2.10s** | **5.00s** | **26.80 GB** | **0.00 GB** | **400.4s** | **520.1s** | **85.4s** |
| **60** | **`gvisor-swap-pool`** | **60 / 60 (100%)** | **1.97s** | **4.48s** | **20.58 GB** | **0.00 GB** | **0.10s** | **2.34s** | **26.07s** |
| **80** | **`gvisor-swap-pool`** | **80 / 80 (100%)** | **1.80s** | **4.65s** | **21.17 GB** | **4.84 GB** | **6.94s** | **25.36s** | **38.04s** |
| **100** | **`gvisor-swap-pool`** | **100 / 100 (100%)** | **1.79s** | **2.92s** | **23.75 GB** | **12.28 GB** | **7.58s** | **37.20s** | **49.27s** |
| **120** | **`gvisor-swap-pool`** | **120 / 120 (100%)** | **2.05s** | **3.73s** | **23.06 GB** | **20.70 GB** | **12.31s** | **61.39s** | **77.39s** |
| **140** | **`gvisor-swap-pool`** | **140 / 140 (100%)** | **2.43s** | **7.47s** | **23.38 GB** | **23.15 GB** | **13.90s** | **66.59s** | **103.75s** |
| **160** | **`gvisor-swap-pool`** | **160 / 160 (100%)** | **2.19s** | **4.26s** | **22.18 GB** | **29.42 GB** | **24.07s** | **92.54s** | **121.49s** |
| **180** | **`gvisor-swap-pool`** | **180 / 180 (100%)** | **2.59s** | **6.43s** | **24.85 GB** | **30.29 GB** | **29.72s** | **110.83s** | **157.86s** |
| **200** | **`gvisor-swap-pool`** | **200 / 200 (100%)** | **2.79s** | **5.45s** | **24.24 GB** | **33.95 GB** | **25.83s** | **79.17s** | **123.22s** |
| **220** | **`gvisor-swap-pool`** | **220 / 220 (100%)** | **2.74s** | **6.08s** | **22.37 GB** | **31.58 GB** | **63.55s** | **263.34s** | **216.26s** |
| **240** | **`gvisor-swap-pool`** | **240 / 240 (100%)** | **3.43s** | **12.83s** | **23.15 GB** | **48.78 GB** | **69.47s** | **188.56s** | **274.42s** |

---

## 5. Key Technical Takeaways

### A. Native `runc` Performance & Density Ceiling
1.  **200% Density Boost (80 -> 240 Sandboxes @ 100% Pass Rate):**
    *   **Without Swap (`baseline-pool`):** Hits a hard physical wall at 100 sandboxes, causing a complete node memory crash (`Node NotReady`). The maximum safe capacity is only 80 sandboxes.
    *   **With Local NVMe SSD Swap (`lssd-swap-pool`):** Scales cleanly all the way to **240 concurrent sandboxes (100% Pass Rate)**, offloading **`42.79 GB`** of dormant memory out to disk while maintaining blistering fast **1.39s execution speed**!
2.  **Low Memory Stall Pressure up to 240 Density:** Memory stall pressure (`mem_psi`) remains minimal (under 14s cumulative during startup), preserving fast execution responsiveness.

### B. Secure `gvisor` (`runsc`) Performance & Sentry Swapability
1.  **200% Density Boost (80 -> 240 Sandboxes @ 100% Pass Rate):**
    *   **Without Swap (`gvisor-baseline-pool`):** Fails fatally at 100 sandboxes, crashing the node (`Node NotReady`) because each gVisor pod uses `~441 MB` of RAM (375 MB Pandas DataFrame + **~65 MB Sentry user-space kernel RAM**), fully exhausting the physical RAM limit.
    *   **With Local NVMe SSD Swap (`gvisor-swap-pool`):** Scales cleanly to **240 concurrent sandboxes (100% Pass Rate)**, offloading **`48.78 GB`** of RAM out to disk! Execution speed degrades gracefully to **3.43s** (12.83s P99) under heavy NVMe paging load.
2.  **100% Sentry Kernel Swapability:**
    *   Because gVisor's Sentry binary runs as a Linux user-space process, **Linux `kswapd` pages out gVisor's Sentry kernel memory out to disk alongside Python memory**, allowing 240 sandboxes to run reliably on a single 30 GB Node!

### C. Shared Failure Mode & Hardware Limits
1.  **Node-Level Crash vs Graceful Degradation (Why 100 Baseline Fails):**
    *   **Without Swap:** Surpassing physical RAM capacity causes Kubelet to freeze and fail heartbeats, resulting in a catastrophic `Node NotReady` crash before individual pods can be gracefully OOM-killed.

### D. Core Isolation, Deployment Pacing & CPU Overcommitment
1.  **Core Isolation & Deployment Pacing:**
    *   **Node Core Isolation (`node-tuner-ds`):** Pins Kubelet and containerd system daemons to Cores 0 and 1, preventing system daemons from competing for CPU against sandbox workers and maintaining sub-2-second P99 execution latency at high densities.
    *   **Orchestrator-Level Staggering:** Moving deployment delays to a 1.8s Go test runner loop provides `kswapd` a continuous 1.8s window per pod to write pages out to the Local NVMe SSD, eliminating Page Cache thrashing storms on boot.
2.  **Massive CPU Overcommitment (`15m` Requests):**
    *   By lowering the CPU request to `15m` per sandbox, we allow the Kubernetes scheduler to pack hundreds of sandboxes onto an 8-core node. Because the active execution takes only ~1 second and the rest of the time is spent idle, we can safely overcommit CPU limits and rely on the Linux kernel to burst CPU allocation dynamically when a sandbox wakes up. If we don't include a CPU request, the pods schedule infinitely but face catastrophic CPU starvation when computing simultaneously. Without guaranteed `cpu.shares`, execution times skyrocket, causing the benchmark to fail due to execution timeouts.