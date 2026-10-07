# Tier 1: suspend / resume (disk-only sleep)

Verifies the P0 requirement at the cheap tier: **sleep releases the pod's
CPU/memory; wake is a pod recreation measured in seconds**. Live process
memory is lost; everything on the late-bound Filestore workspace survives,
and so does the per-sandbox Service (stable DNS across the cycle). No GKE
Pod Snapshots involved — this works on any cluster. For memory-intact
hibernation, see [30-snapshot-hibernate.md](30-snapshot-hibernate.md).

`spec.operatingMode` is the dial: it declares intent (`Running` vs
`Suspended`); observed state is reported by conditions. `Suspended=True`
(reason `PodTerminated`) once the pod is gone; `Ready` flips back to `True`
only when the recreated pod is Running with an IP.

## Suspend

Pick an employee and resolve their claim to its Sandbox (pod name = Sandbox
name):

```sh
EMP=alice
SB=$(kubectl -n openclaw-fleet get sandboxclaim "oc-$EMP" -o jsonpath='{.status.sandbox.name}')

kubectl -n openclaw-fleet patch sandbox "$SB" --type merge \
    -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl -n openclaw-fleet wait --for=delete "pod/$SB" --timeout=120s
```

What suspension actually is — **only the pod is gone**:

```sh
kubectl -n openclaw-fleet get pod "$SB"          # NotFound — resources released
kubectl -n openclaw-fleet get svc "$SB"          # per-sandbox Service survives
kubectl -n openclaw-fleet get sandbox "$SB" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
# Suspended=True (PodTerminated), Ready=False (SandboxSuspended)
kubectl -n openclaw-fleet get sandboxclaim "oc-$EMP" -o jsonpath='{.status.podIPs}'
# empty — podIPs are cleared whenever the pod is absent
```

The Filestore workspace volume stays bound to the Sandbox and reattaches on
wake — plant a marker first if you want proof:

```sh
# before suspending:
kubectl -n openclaw-fleet exec "$SB" -- sh -c 'echo "remember me" > /workspace/marker.txt'
```

## Resume, and time the wake

Wake is a pod **recreation from the template** (the warm pool only serves
new claims — an existing Sandbox keeps its identity), so it costs one
scheduling + container-start cycle, not an image pull (the image is already
on the node in the steady state):

```sh
START=$(date +%s)
kubectl -n openclaw-fleet patch sandbox "$SB" --type merge \
    -p '{"spec":{"operatingMode":"Running"}}'
kubectl -n openclaw-fleet wait sandbox "$SB" --for=condition=Ready --timeout=180s
echo "wake-up duration: $(( $(date +%s) - START ))s"
```

For sub-second resolution, read the condition's own timestamp instead of
wall-clocking the wait:

```sh
kubectl -n openclaw-fleet get sandbox "$SB" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}'
```

Verify disk state survived the pod's death:

```sh
kubectl -n openclaw-fleet exec "$SB" -- cat /workspace/marker.txt   # -> remember me
```

## Notes

- **It is safe to flip back to `Running` while suspension is still in
  progress** — never wait for `Suspended=True` before resuming. A platform
  that wakes on connect holds the user's first request during this window.
- The controller does not currently remove the `Suspended` condition on
  resume, so a stale `Suspended` entry may linger; treat `Ready` as the
  authoritative signal, not the mere presence of `Suspended`.
- If the Tier 2 snapshot policy ([10-policy.yaml](10-policy.yaml)) is
  installed and a Ready snapshot exists for this sandbox, the recreated pod
  will restore from it — that is Tier 2 behavior. To measure a pure Tier 1
  (cold-process) wake, run this before taking any snapshot of the sandbox,
  or delete its snapshots first.
