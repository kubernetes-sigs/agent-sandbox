# Container Isolation Runtimes on GKE with Local SSD Swap

This directory contains configuration, deployment scripts, and a comprehensive performance benchmark study for running high-density `agent-sandbox` workloads across sandboxed container runtimes on Google Kubernetes Engine (GKE):

1. **gVisor (`runsc`)**: User-space kernel application sandboxing via syscall interception (Sentry & Gofer).
2. **Kata Containers (`kata-qemu`)**: Hardware-virtualized MicroVMs using the QEMU hypervisor.
3. **Kata Containers (`kata-clh`)**: Hardware-virtualized MicroVMs using the Cloud Hypervisor (CLH) Rust-based VMM.

All metrics reference benchmarks conducted on **`c4-standard-8`** GKE node instances (8 vCPUs, 30 GB physical RAM, ~28 GB allocatable).

---

## 1. Architectural Profiles

| Feature | gVisor (`runsc`) | Kata (`kata-qemu`) | Kata (`kata-clh`) |
| :--- | :--- | :--- | :--- |
| **Isolation Level** | User-space emulated kernel | Hardware-virtualized MicroVM | Hardware-virtualized MicroVM |
| **VMM / Kernel Model** | Simulated kernel interface (`Sentry`) | Monolithic QEMU hypervisor & guest kernel | Async Rust Cloud Hypervisor VMM & guest kernel |
| **Security Boundary** | Strong (restricts syscall access) | Strictest (nested virtualization) | Strictest (nested virtualization) |
| **Memory Sharing** | Low (emulated address translation maps) | Lowest (hard boundaries between QEMU hypervisors) | Lowest (hard boundaries between CLH hypervisors) |
| **VMM Config Protocol** | N/A (process shim) | Synchronous serial vCPU hotplugging | Single async HTTP PUT payload |

---

## 2. Host Memory Footprint per Pod

Process-level physical memory (Resident Set Size - RSS) and incremental host RSS measured across sequential pod deployments on a clean cluster:

| Runtime | Host RSS per Pod | Incremental Host RSS | Process Component Breakdown |
| :--- | :--- | :--- | :--- |
| **gVisor (`runsc`)** | **~370 MiB** | **~206.0 MiB** | <ul><li>`runsc-sandbox` (Sentry): ~311 MiB</li><li>`runsc-gofer` (FS proxy): ~25 MiB</li><li>Control shim: ~34 MiB</li></ul> |
| **Kata (`kata-qemu`)** | **~1,130 MiB** | **~631.5 MiB** | <ul><li>`qemu-system-x86_64` (VMM): ~678 MiB</li><li>`virtiofsd` (FS daemon): ~421 MiB</li><li>Control shim: ~35 MiB</li></ul> |
| **Kata (`kata-clh`)** | **~1,128 MiB** | **~687.5 MiB** | <ul><li>`cloud-hypervisor` (VMM): ~664.7 MiB</li><li>`virtiofsd` (FS daemon): ~426.8 MiB</li><li>Control shim: ~36.0 MiB</li></ul> |

---

## 3. Node Tuning & CPU Isolation Strategy

To push density ceilings higher, a custom node-tuning DaemonSet was developed and applied across the node pools:

1. **CPU Isolation (`--reserved-cpus=0,1`)**: Linux cgroups and Kubelet static CPU management reserve Cores 0 and 1 strictly for host system daemons and Kubelet, allowing workload containers to run on Cores 2–7 without starving Kubelet of heartbeat check cycles.
2. **`systemd-journald` Storage Rate-Limiting**: `systemd-journald` log storage is rate-limited (`RateLimitIntervalSec=30s`, `RateLimitBurst=1000`) to eliminate logging CPU spikes during boot storms.
3. **Kernel ARP Table Scaling**: Linux kernel ARP garbage collection thresholds (`net.ipv4.neigh.default.gc_thresh1/2/3`) are scaled from `128/512/1024` to `2048/4096/8192` to eliminate packet drops across hundreds of internal container veth pairs.

### Performance Impact of Node Tuning:
- **gVisor (`runsc`)**: Expanded maximum stable density on NVMe SSD swap from 140 pods (untuned) to **160 pods (tuned)**, while reducing Sandbox Ready P99 latency by ~60% and Chrome Ready P99 latency by ~40%.
- **Kata Cloud Hypervisor (`kata-clh`)**: Expanded maximum stable density on NVMe SSD swap from 40 pods (untuned) to **50 pods (tuned)** with 100% success.

---

## 4. High Density Scaling Thresholds

Summary comparison of maximum stable pod density and startup latency (P50/P99 Chrome Ready) across baseline (No Swap) and NVMe Local SSD Swap node pools:

| Runtime | Config Type | Max Stable Density | Chrome Ready P50 (Avg) | Chrome Ready P99 |
| :--- | :--- | :--- | :--- | :--- |
| **gVisor (`runsc`)** | No Swap (Tuned) | **80 Pods** | **17.59s** | **41.07s** |
| **gVisor (`runsc`)** | NVMe SSD Swap (Tuned) | **160 Pods** | **59.02s** | **214.95s** |
| | | | | |
| **Kata (`kata-qemu`)** | No Swap (Tuned) | **20 Pods** | **16.44s** | **24.92s** |
| **Kata (`kata-qemu`)** | NVMe SSD Swap | **20 Pods** | **16.16s** | **24.28s** |
| | | | | |
| **Kata (`kata-clh`)** | No Swap (Tuned) | **40 Pods** | **34.54s** | **60.99s** |
| **Kata (`kata-clh`)** | NVMe SSD Swap (Tuned) | **50 Pods** | **71.02s** | **174.14s** |

---

## 5. Key Takeaways: The Impact of Local SSD Swap

GKE Memory Swap on dedicated Local SSDs significantly extends node capacity for memory-heavy sandbox workloads, but its effectiveness depends heavily on the underlying runtime architecture:

- **gVisor (`runsc`) — +100% Density Increase (80 → 160 Pods)**: Swap doubles the maximum stable density. Without swap, gVisor Sentry user-space kernels exhaust host RAM at 80 pods. NVMe Local SSD Swap absorbs cold anonymous memory pages from idle Chrome processes, allowing gVisor to scale reliably to 160 pods. Scaling beyond 160 pods (to 180+) shifts the bottleneck from memory capacity to NVMe write queue depth saturation.
- **Kata Cloud Hypervisor (`kata-clh`) — +25% Density Increase (40 → 50 Pods)**: Swap extends stable density from 40 to 50 pods with 100% success. Cloud Hypervisor's async Rust architecture decouples VMM communications from guest vCPUs (preventing I/O stalls). At 60 pods, physical CPU cores become the primary bottleneck due to parallel hypervisor scheduling and pageout workloads.
- **Kata QEMU (`kata-qemu`) — No Density Gain (Stuck at 20 Pods)**: Swap does not improve density for QEMU microVMs. When host OS pages VM blocks to NVMe swap, guest page faults force uninterruptible disk reads (D-state). In QEMU's legacy monolithic architecture, the Big QEMU Lock (BQL) causes vCPU threads in D-state to lock up the main VMM event loop, triggering containerd-shim keep-alive timeouts.

---

## 6. Failure Modes Breakdown

### gVisor (`runsc`)
- **Untuned Node**: Emulating syscalls for high-density Chrome instances consumes high host CPU. Emulation threads compete with system services, starving Kubelet of CPU cycles until GKE flags the node `NotReady`.
- **Tuned Node + NVMe Swap**: CPU isolation protects Kubelet on Cores 0–1, enabling scaling to 160 pods with 100% success. At 180 pods, swapping ~25.5 GB of memory pages saturates the NVMe SSD controller write queues, causing Kubelet to block in D-state attempting log writes and triggering workload failures (85.56% success).

### Kata Containers (`kata-qemu`)
- **Host Boot Disk Sizing**: Chrome sparse cache files (`/tmp`) generate 80 GB+ of writes across 40+ sandboxes. On default 100 GB boot disks, this triggers host disk pressure GC and crashes `virtiofsd` with `SIGBUS` (exit code 135). Node boot disks must be sized to **≥150–250 GB**.
- **Big QEMU Lock (BQL) Deadlock**: When host OS pages VM blocks to NVMe swap, subsequent guest page faults force uninterruptible disk reads (D-state). In QEMU's monolithic thread synchronization model, a vCPU thread in D-state locks up the main VMM event loop, failing containerd-shim keep-alive pings within 15 seconds.

### Kata Containers (`kata-clh`)
- **Async Rust Architecture**: Cloud Hypervisor decouples the VMM control channel from guest vCPU execution threads, reducing D-state stalls to 0.069% (13x fewer than QEMU).
- **Fast Config Payload**: CLH boots via a single HTTP PUT request payload instead of QEMU's serial vCPU hotplugging, allowing **40 pods baseline** (2.5x density over QEMU without swap).
- **CPU Bottleneck at 60 Pods**: With swap enabled and Kubelet protected on Cores 0–1, `kata-clh` reaches **50 pods with 100% success**. At 60 pods, the 6 physical workload cores are completely overwhelmed by 60 concurrent VMM hypervisor loops, stretching VM boot times past 3 minutes and causing timeouts.

---

## Subdirectory Navigation

Explore detailed runtime configuration, density sweep data, and setup instructions for each runtime:

- [**gVisor Guide & Benchmarks**](./gVisor/README.md) — Detailed gVisor execution guide, Sentry memory analysis, 60–160 density sweep matrix (180 failure threshold), and node tuner configuration.
- [**Kata Containers Guide & Benchmarks (`kata-qemu` & `kata-clh`)**](./kata/README.md) — Comprehensive Kata guide covering QEMU vs Cloud Hypervisor VMM comparison, 20–60 density sweeps, and memory limit tuning.
- [**Default `runc` Baseline Guide**](../README.md) — Main GKE Local SSD Swap example landing page and native container baseline.
