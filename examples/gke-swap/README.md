# High-Density Agent Sandbox on GKE with Local SSD Swap

This example demonstrates how to configure Google Kubernetes Engine (GKE) Memory Swap using dedicated Local SSDs to drastically increase the density of `agent-sandbox` workloads (specifically Chromium pods) on a single node.

By enabling swap on a dedicated Local SSD, we can ensure 100% workload success up to the **200 pods per node** on a cost-effective `c4-standard-8` instance (8 vCPUs, 30 GB RAM), whereas the baseline pool collapses and experiences severe workload instability starting from 130 pods per node.

## Why Swap for Agent Sandboxes?

AI agent workloads often require running web browsers (like Chrome) to interact with web pages. Chrome instances are memory-intensive but often have large amounts of idle memory (e.g., background tabs, unused allocations). 

In a standard Kubernetes cluster, when physical memory is exhausted, the node's Out-Of-Memory (OOM) killer will terminate pods. 

By enabling swap on fast, local NVMe SSDs (Local SSDs), GKE can swap out idle memory pages, allowing you to overcommit memory safely and pack significantly more pods onto each node.

## Container Runtimes & Isolation Options

Depending on your security and isolation requirements, `agent-sandbox` supports multiple container isolation runtimes on GKE. We have conducted high-density swap benchmarks across three major runtimes:

1. **Default `runc` (Process Isolation)**: Standard Linux container isolation. Lowest overhead, highest density (**200 pods/node** with swap, +66% gain).
2. **gVisor `runsc` (Syscall Interception)**: User-space kernel sandboxing. Strong process isolation, medium density (**160 pods/node** with swap, +100% gain).
3. **Kata Containers (`kata-clh` / `kata-qemu`) (MicroVMs)**: Hardware-assisted nested virtualization. Maximum VM-level security boundary (**50 pods/node** with swap under `kata-clh`, **20 pods/node** under `kata-qemu`).

> [!TIP]
> **Cross-Runtime Benchmark Summary**: For a comprehensive side-by-side comparison matrix, memory footprint analysis, and architectural trade-offs across runtimes, visit the [**Container Isolation Runtimes Overview**](./runtimes/README.md).

---

## Vanilla `runc` Performance Results

### Moderate Density Benchmarks on `c4-standard-8` (30 GB RAM, 200 Pods)

We evaluated and compared two node pools on GKE using `c4-standard-8` instances, running a concurrent density sweep of **120, 160, 200, and 240 pods**:

1. **Baseline Pool**: `c4-standard-8` (No Swap)
2. **LSSD-Swap Pool**: `c4-standard-8-lssd` (Swap enabled on dedicated Local SSD)

Both pools configured the Chrome Sandbox pods with a standard **Burstable QoS** profile requesting **150 MiB** of memory (with a 2 GiB limit). Note that these experiments were conducted using vanilla pods (default runc runtime) to establish a baseline.

The table below shows the P99 `ChromeReady` latency (the time it takes for the Chrome process inside the sandbox to become responsive and ready for the agent) as we scale the number of concurrent pods per node:

| Pod Density | No Swap Healthy Pod | No Swap Latency (P99) | Swap Healthy Pod | Swap Latency (P99) | Comparison |
| :---------- | :------------------ | :-------------------- | :--------------- | :----------------- | :--------- |
| 120         | **120 / 120** (100%)| 61 s                  | **120 / 120** (100%) | 58 s               | Both healthy; comparable latency. |
| 160         | 159 / 160 (99.4%)   | —                     | **160 / 160** (100%) | 181 s              | **Swap Advantage**: Swap maintains 100% stability; Baseline starts dropping pods. |
| **200**     | 149 / 200 (74.7%)   | —                     | **200 / 200** (100%) | 268 s              | **Clear Threshold**: Baseline collapses (25% loss); Swap maintains 100% stability. |
| 240         | 111 / 240 (46.4%)   | —                     | 170 / 240 (71.0%)    | —                  | **Limit Reached**: Both pools experience failures, but Swap keeps 71% alive vs Baseline's 46%. |

#### Key Takeaways
* **66% Density Increase**: Safely run **200 pods** instead of **120 pods** on the exact same hardware.
* **Clear Collapse Threshold**: Without swap, the node collapses at 140 pods. With swap, the node gracefully offloads idle memory to the Local SSD, keeping workload alive and healthy.

#### Memory Dynamics & Thrashing Thresholds

Through direct measurement, we have determined that a Chrome sandbox pod typically consumes between 200 MiB and 250 MiB of memory at steady state. 

Given that a `c4-standard-8` node provides approximately **28 GiB** of allocatable memory:
*   At **200 MiB/pod (average)**, the physical memory limit is reached at **~140 pods** (`140 * 200 MiB = 28 GiB`).

Consequently, we expect the baseline pool (no swap) to start experiencing severe thrashing or crashes within the **110 to 140 pod range**, depending on the activity level of the pods and page cache usage.

#### The Fluctuating Nature of Swap & Methodology

Because of these dynamics, memory pressure is not a static number. In our experiments, we observed some variability in single-run tests:
*   In some runs, the baseline pool was able to survive up to 170 pods.
*   In other runs, the baseline pool crashed or experienced severe OOMs at 130 or 150 pods.

Repeating the tests multiple times, the aggregated data clearly shows that while the baseline pool *can* occasionally survive higher densities, **~120 pods is the limit for reliable deployments**. Beyond 120 pods, the baseline pool can become unstable or experience high latency. Conversely, the **swap-enabled pool can reliably sustain 200 pods** across all runs.

### High & Extreme Density Benchmarks on `c4-standard-32` (120 GB RAM, 768 Pods)

We evaluated baseline vs LSSD swap configurations on `c4-standard-32` instances (120 GB RAM) and tested densities up to 1024 pods.

#### 1. The Critical Role of Node Tuning
The high-density results detailed in the metrics below (particularly reaching 768 stable pods) are **only achievable when utilizing the provided [`node-tuner-daemonset.yaml`](node-tuner-daemonset.yaml)**. 

**Why is tuning required?**
At extreme pod densities, system daemons experience heavy CPU churn handling pod creation, CRI/CNI setup, and health checks. Without CPU isolation, these pod initialization loops starve Kubelet of compute cycles, causing it to drop heartbeats and trigger node watchdog crashes. Additionally, rapid cgroup events and container thrashing flood `systemd-journald`, which creates high CPU load and saturates boot disk write queues.

**How the Node Tuner fixes this:**
To prevent system lockups during extreme pod density tests, the [`node-tuner-daemonset.yaml`](node-tuner-daemonset.yaml) configuration must be applied:
*   **Dynamic CPU Isolation:** Expands CPU pinning dynamically to ensure Kubelet and system daemons have enough compute cycles.
*   **ARP Cache Tuning:** Increases ARP cache thresholds (`gc_thresh` 2048/4096/8192) to accommodate the massive number of pod IP addresses routing through the node.
*   **Advanced Logging Mitigation:** Applies `systemd-journald` rate limits (`SystemMaxUse=500M`, `RateLimitIntervalSec=30s`, `RateLimitBurst=1000`) and redirects journal logging to NVMe Local SSDs (bind-mounting `/var/lib/containerd/systemd-journal` onto `/var/log/journal`) to prevent logging loops from overwhelming system resources.


#### 2. Performance Metrics (256, 512, 768, and 1024 Densities)
The table below compares the baseline and LSSD swap node pools under a 1000ms staggered creation pacing.

| Scenario / Pool | Density | Sandbox Ready (Avg/P99) | Chrome Ready (Avg/P99) | Total Time (Avg/P99) |
| :--- | :--- | :--- | :--- | :--- |
| **baseline-pool** | 256 | 1.62s / 2.65s | 2.75s / 3.86s | 2.75s / 3.86s |
| **lssd-swap-pool** | 256 | **1.47s / 7.03s** | **2.58s / 8.15s** | **2.58s / 8.15s** |
| **baseline-pool** | 512 | 95.86s / 789.12s | 97.53s / 791.60s | 97.53s / 791.60s |
| **lssd-swap-pool** | 512 | **1.92s / 6.37s** | **3.09s / 7.77s** | **3.09s / 7.77s** |
| **baseline-pool** | 768 | *Failed* (timeout) | *Failed* (timeout) | *Failed* (timeout) |
| **lssd-swap-pool** | 768 | **4.51s / 35.10s** | **6.00s / 37.69s** | **6.00s / 37.69s** |
| **baseline-pool** | 1024 | *Failed* | *Failed* | *Failed* |
| **lssd-swap-pool** | 1024 | *Failed* | *Failed* | *Failed* |

#### 3. Final Recommendations & Conclusions
1.  **512 Pods pushes the limits without swap**: In standard network PD pools, running 512 pods saturates physical memory allocations and triggers active page cache evictions. This pushes the node to the edge of stability due to slow disk-wait queues on standard PD.
2.  **Significant Density Improvement (512 -> 768 Pods)**: By combining NVMe Local SSD swap with node-level CPU isolation and ARP cache tuning, we achieved a **50% increase in stable pod density** (from 512 pods up to 768 pods per node) without node crashes. 
3.  **Local SSDs prevent Page Cache Bottlenecks**: Even when swap usage is 0B, if memory pressure evicts file-backed page caches (Chrome binaries), re-reading them from NVMe Local SSDs (mounted at `/var/lib/containerd`) takes milliseconds. On baseline PD pools, reading evicted binaries back from network disks saturates the 3,000 IOPS queue and causes watchdog timeouts.
4.  **CPU isolation is mandatory**: Reserve at least 2 cores (`reservedSystemCPUs: "0,1"`) for Kubelet and OS services. For 768+ active pods on `c4-standard-32` under swap, reserve **8 cores** (`reservedSystemCPUs: "0-7"`).


## Run the Example

### Prerequisites

- GKE Cluster version **1.34.1-gke.1341000** or later.
- Google Cloud CLI (`gcloud`) installed and configured.
- `kubectl` installed.
- A Google Cloud project with sufficient quota for `C4` machine types and Local SSDs.

### Step 1: GKE Swap Configuration

We use [`swap-dedicated-lssd.yaml`](swap-dedicated-lssd.yaml) to configure the node with a dedicated Local SSD exclusively for swap:

```yaml
linuxConfig:
  swapConfig:
    enabled: true
    dedicatedLocalSsdProfile:
      diskCount: 1
  sysctl:
    vm.watermark_scale_factor: 500
    vm.swappiness: 100
```

*   **`swapConfig.enabled: true`**: Enables node memory swap, allowing the node to allocate swap space to Burstable pods.
*   **`dedicatedLocalSsdProfile.diskCount: 1`**: Dedicates the single available Local SSD on the `c4-standard-8-lssd` instance entirely to swap.
*   **`vm.swappiness: 100`**: Instructs the kernel to aggressively swap out idle anonymous memory pages (highly effective for idle Chrome processes). The sysctl parameters are just for reference. We expect follow-up PRs to provide tuned and more performant settings.
*   **`vm.watermark_scale_factor: 500`**: Tells the kernel to begin background page reclamation much earlier (at 5% of memory instead of the default 0.1%), smoothing out memory pressure transitions and preventing sudden allocation spikes from triggering direct reclaim stalls or OOMs. The sysctl parameters are just for reference. We expect follow-up PRs to provide tuned and more performant settings.

### Step 2: Deploy the GKE Cluster

We provide a helper script [`deploy_cluster.sh`](deploy_cluster.sh) that automates cluster provisioning by creating both the baseline node pool and the Memory Swap enabled node pool.

For standard moderate density testing (`c4-standard-8`):

```bash
chmod +x deploy_cluster.sh
./deploy_cluster.sh
```

For high-density benchmarking (`c4-standard-32`):

```bash
MAX_PODS_PER_NODE=1024 BASELINE_MACHINE_TYPE="c4-standard-32" SWAP_MACHINE_TYPE="c4-standard-32-lssd" ./deploy_cluster.sh
```

### Step 3: Optional Node Tuning for High-Density Tests (>256 Pods)

If you plan to run extreme density sweeps (512–1024 pods per node), apply the provided [`node-tuner-daemonset.yaml`](node-tuner-daemonset.yaml) to configure CPU isolation, ARP cache expansion, and journald log rate-limiting, then wait for the rollout to complete:

```bash
kubectl apply -f node-tuner-daemonset.yaml
kubectl rollout status daemonset/node-tuner-ds -n kube-system
```

### Step 4: Configure the Sandbox Workloads

Kubernetes Limited Swap requires **Burstable QoS** pods. Configure your sandboxes with a memory request and a higher limit. In `LimitedSwap` mode, the swap limit allocated to a container is proportional to its memory request, calculated as:
`Swap Limit = (Container Memory Request / Node Allocatable Memory) * Node Allocatable Swap`
This means that pods with larger memory requests will be allocated more swap space (rationed proportionally), while pods with very small requests might not get enough swap to be useful. You can refer to [`test/e2e/extensions/chromesandbox_density_test.go`](../../test/e2e/extensions/chromesandbox_density_test.go) for more details.

Example `SandboxTemplate`:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata:
  name: chrome-swap-template-burstable
spec:
  podTemplate:
    spec:
      nodeSelector:
        cloud.google.com/gke-nodepool: lssd-swap-pool
      containers:
      - name: chrome-sandbox
        image: us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/chrome-sandbox:latest-main
        resources:
          requests:
            memory: "100Mi"
          limits:
            memory: "2Gi"
```

---

### Step 5: Run the Performance/Density Tests

Use the provided [`run_chromesandbox_density_test.sh`](run_chromesandbox_density_test.sh) script to run the density sweep.

For moderate density testing on `c4-standard-8` (`120 160 200 240` pods):

```bash
chmod +x run_chromesandbox_density_test.sh
./run_chromesandbox_density_test.sh
```

For high-density benchmarking on `c4-standard-32` (`256 512 768 1024` pods, requires node-tuner applied):

```bash
DENSITIES="256 512 768 1024" ./run_chromesandbox_density_test.sh
```

#### Results:
- Raw timing metrics are saved to `artifacts/<scenario>/<density>/.../density_metrics.json`.
- A compiled performance results table is written to `artifacts/perf_test_summary.md`.

## Alternative Runtimes

For gVisor or Kata Containers deployments and benchmarks, refer to:
- [**`runtimes/README.md`**](./runtimes/README.md) — Multi-runtime comparison summary
- [**`runtimes/gVisor/README.md`**](./runtimes/gVisor/README.md) — gVisor setup & results (160 pods max stable density)
- [**`runtimes/kata/README.md`**](./runtimes/kata/README.md) — Kata Containers setup & results (`kata-clh` & `kata-qemu`, up to 50 pods (kata-clh) / 20 pods (kata-qemu))