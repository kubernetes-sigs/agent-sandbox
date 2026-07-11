# Windows Sandbox Example

This example demonstrates how to run a Windows guest inside the Agent Sandbox using the [`dockurr/windows`](https://hub.docker.com/r/dockurr/windows) image from the [`dockur/windows`](https://github.com/dockur/windows) project. The container runs KVM/QEMU to boot a full Windows OS (Windows 11 by default) nested inside a Linux container.

## Prerequisites

-   A Kubernetes cluster with **KVM support** on at least one node. The host must expose `/dev/kvm` and `/dev/net/tun` (bare-metal or VMs with nested virtualization enabled, e.g., GCP, Azure, Proxmox).
-   `agent-sandbox` controller installed.
-   Sufficient node resources: at least 2 CPU cores and 4 GiB memory per sandbox pod (4 CPU / 8 GiB recommended).

### Verifying KVM support

On the target node, confirm the devices exist:

```bash
ls -la /dev/kvm /dev/net/tun
```

> **Note**: This example **does not work on Kind** — KVM is typically not available inside the Kind node container.

## Usage

1.  Apply the Sandbox resource:
    ```bash
    kubectl apply -f windows-sandbox.yaml
    ```

2.  Wait for the pod to be ready. The first boot downloads and installs Windows, which can take **10–30 minutes** depending on network speed and node performance. Subsequent boots are fast because the system disk persists on the PVC.
    ```bash
    kubectl wait --for=condition=ready pod --selector=sandbox=windows-sandbox --timeout=30m
    ```

3.  **Access the Windows guest**:

    **Web console (noVNC)** — no client needed:
    ```bash
    kubectl port-forward pod/windows-sandbox 8006:8006
    ```
    Then open [http://localhost:8006](http://localhost:8006) in your browser.

    **RDP** — native remote desktop:
    ```bash
    kubectl port-forward pod/windows-sandbox 3389:3389
    ```
    Then connect your RDP client to `localhost:3389`.

    **VNC**:
    ```bash
    kubectl port-forward pod/windows-sandbox 5900:5900
    ```

## Exposed Ports

| Port  | Protocol | Service                    |
|-------|----------|----------------------------|
| 8006  | TCP      | noVNC web console          |
| 3389  | TCP/UDP  | RDP (Remote Desktop)       |
| 5900  | TCP      | VNC                        |

## Configuration

### Environment variables

| Variable    | Value  | Description                                  |
|-------------|--------|----------------------------------------------|
| `VERSION`   | `11`   | Windows version to install (e.g., `11`, `10`, `2022`) |
| `DISK_SIZE` | `64G`  | Size of the Windows system disk              |

Additional variables can be added to the manifest. See the [dockur/windows documentation](https://github.com/dockur/windows) for the full list, including `USERNAME`, `PASSWORD`, `LANGUAGE`, `REGION`, `KEYBOARD`, `CPU_CORES`, and `RAM_SIZE`.

### Resources and storage

The sandbox pod requests 2 CPU / 4Gi memory and limits to 4 CPU / 8Gi memory. A 64 GiB PVC is mounted at `/storage` to persist the Windows system disk across pod restarts. Adjust these values in the manifest to match your workload needs. Deleting the Sandbox will also delete the PVC and all Windows data.

## Limitations

-   **KVM required**: Requires `/dev/kvm` on the host node. Does not work on Kind or most managed Kubernetes services without nested virtualization. For arm64 Windows, use the separate [`dockur/windows-arm`](https://github.com/dockur/windows-arm) image instead.
-   **Privileged container**: Requires `privileged: true` (which implicitly grants all capabilities including `NET_ADMIN`, needed for TAP device management) and `hostPath` device mounts (`/dev/kvm`, `/dev/net/tun`). No explicit `capabilities.add` is needed. Clusters enforcing the `restricted` or `baseline` Pod Security profile, or policy engines (Kyverno, OPA Gatekeeper, VAP), will block this manifest. Use a namespace with the `privileged` Pod Security profile.
-   **`hostPath` is node-specific**: The pod will only schedule on nodes that have `/dev/kvm` and `/dev/net/tun`. If needed, add a `nodeSelector` or `nodeAffinity` to target specific nodes.

## Troubleshooting

-   **Pod stuck in `ContainerCreating`**: Run `kubectl describe pod windows-sandbox` and check for events related to `/dev/kvm` or `/dev/net/tun`.
-   **Web console shows black screen**: Windows may still be installing. Wait a few more minutes and refresh.
-   **`/dev/kvm` permission denied**: Verify the host device exists and the node's security policy allows privileged containers.

## References

-   [dockur/windows](https://github.com/dockur/windows) — Run Windows inside a Docker container via KVM/QEMU
-   [dockur/windows on Docker Hub](https://hub.docker.com/r/dockurr/windows) — Container image and tags
-   [dockur/windows environment variables](https://github.com/dockur/windows?tab=readme-ov-file#-how-to-use) — Full configuration reference (`VERSION`, `USERNAME`, `PASSWORD`, `LANGUAGE`, `CPU_CORES`, `RAM_SIZE`, etc.)
-   [dockur/windows-arm](https://github.com/dockur/windows-arm) — ARM64 variant for Windows on arm64 hosts
-   [dockur/windows Kubernetes example](https://github.com/dockur/windows/blob/master/kubernetes.yml) — Official Kubernetes manifest from the upstream project
