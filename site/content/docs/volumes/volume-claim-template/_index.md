---
title: "volumeClaimTemplates"
linkTitle: "volumeClaimTemplates"
weight: 15
description: >
  This guide shows how to use `volumeClaimTemplates` in `SandboxTemplate`.
---
## The Purpose of Sandbox Volumes
By default, containers are ephemeral, meaning any data generated or modified inside a Sandbox is lost when the Pod terminates. 

The introduction of **`volumeClaimTemplates`** into the `SandboxTemplate` API solves this by allowing each Sandbox instance to dynamically provision its own Persistent Volume Claim (PVC). Using semantics identical to a Kubernetes `StatefulSet`, this functionality allows you to define a storage template once, and have the Sandbox controllers automatically create, attach, and manage independent storage volumes for every Sandbox spawned from that template.

**Common Use Cases:**
* **Caching dependencies:** Storing downloaded packages (like `node_modules` or `pip` caches) across agent runs to speed up executions.
* **Preserving state:** Maintaining logs, build artifacts, or internal agent databases uniquely tied to specific sandbox sessions.
* **Warm Pools with State:** Pre-warming sandboxes (`SandboxWarmPool`) with attached storage so they are immediately ready to perform heavy I/O operations without waiting for initial storage provisioning.

---

## How It Works Under the Hood
1. You define a `volumeClaimTemplates` list inside your `SandboxTemplate`.
2. When a `SandboxClaim` requests a sandbox from this template (or a `SandboxWarmPool` provisions a new one), the volume claim templates are seamlessly propagated to the individual `Sandbox` Custom Resources.
3. The Sandbox controller then provisions the associated PVCs and merges these PVCs into the underlying Pod specifications, mounting them securely inside the container. 

---

### Create a SandboxTemplate with Volume Claims
To use a volume, you need to add the `volumeClaimTemplates` array to your `SandboxTemplate` specification and reference it in the `volumeMounts` of your container.

Create a file named `sandbox-template-with-volume.yaml`:

```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: stateful-sandbox-template
spec:
  podTemplate:
    spec:
      containers:
      - name: sandbox-agent
        image: ubuntu:latest
        command: ["sleep", "infinity"]
        # 2. Reference the exact name of the volume claim here
        volumeMounts:
        - name: agent-data
          mountPath: /data
  # 1. Define the dynamic volume configuration here
  volumeClaimTemplates:
  - metadata:
      name: agent-data
    spec:
      accessModes: 
        - ReadWriteOnce
      resources:
        requests:
          storage: 1Gi
```

### Apply the Template
Apply the template to your Kubernetes cluster:
```bash
kubectl apply -f sandbox-template-with-volume.yaml
```

### Spawn a Sandbox using a SandboxClaim
Now that your template is registered, you can spawn an actual sandbox instance by creating a `SandboxClaim`. 

Create a file named `sandbox-claim.yaml`:
```yaml
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxClaim
metadata:
  name: my-stateful-sandbox
spec:
  sandboxTemplateRef:
    name: stateful-sandbox-template
```

Apply the claim:
```bash
kubectl apply -f sandbox-claim.yaml
```

### Verify the Automatically Provisioned Storage
When you applied the `SandboxClaim`, the Sandbox controller automatically intercepted the `volumeClaimTemplates` definition, created a dedicated Persistent Volume Claim, and attached it to your Sandbox's Pod.

You can verify the volume was successfully created by listing your PVCs:
```bash
kubectl get pvc
```
*You should see a newly bound PVC named dynamically based on the Sandbox Pod and the claim name (e.g., `agent-data-<pod-name>`).*

If you execute into the sandbox container, you can verify that the volume is mounted correctly at the `/data` directory:
```bash
kubectl exec -it my-stateful-sandbox -- df -h /data
```

### Validate Data Persistence

To validate the data persistence, destroy the underlying pod to simulate a crash or eviction, and then verify that our data survived when the sandbox controller spins up a replacement.

**1. Write data to the persistent volume**
Execute a command inside your running sandbox container to create a text file within the mounted `/data` directory:
```bash
kubectl exec -it my-stateful-sandbox -- sh -c "echo 'This data will survive a pod crash!' > /data/evidence.txt"
```

**2. Verify the data exists**
Read the file back to ensure it was written successfully:
```bash
kubectl exec -it my-stateful-sandbox -- cat /data/evidence.txt
```

**3. Simulate a failure by deleting the pod**
Because sandboxes are managed by the Sandbox controller, deleting the pod directly simulates an unexpected failure. 
```bash
kubectl delete pod my-stateful-sandbox
```

**4. Wait for the controller to recreate the sandbox**
The Sandbox controller will detect that the pod is missing and automatically recreate it. Because `volumeClaimTemplates` use StatefulSet-like semantics, the controller will reattach the *exact same PVC* to the new pod.
Watch your pods until the new one is running:
```bash
kubectl get pods -w
```

**5. Verify the data survived**
Once the new replacement pod is in the `Running` state, execute into it and read the file again:
```bash
kubectl exec -it my-stateful-sandbox -- cat /data/evidence.txt
```

**The Result:** You should see the text `'This data will survive a pod crash!'` printed in your terminal.

### Python SDK

If you want to use `volumeClaimTemplates` with `k8s_agent_sandbox`, you need to make sure that you re-use the existing sandbox and delete it manually. To run the following an example Python script, you need to build a custom Docker image and apply the SandboxTemplate from [here](https://github.com/kubernetes-sigs/agent-sandbox/tree/main/site/content/docs/volumes/volume-claim-template/source).

Install `k8s_agent_sandbox`:

```bash
pip install k8s_agent_sandbox
```

And run the example Python script:

```python
from k8s_agent_sandbox import SandboxClient

validation_message = "volume validation"

client = SandboxClient()

sandbox1 = client.create_sandbox("simple-sandbox-template")
response1 = sandbox1.commands.run(f"sh -c \"echo '{validation_message}' > /data/volume_validation.txt\"")
print(f"Claim Name: {sandbox1.claim_name}")

client2 = SandboxClient()
sandbox2 = client2.get_sandbox(sandbox1.claim_name)
response2 = sandbox2.commands.run(f"sh -c \"cat /data/volume_validation.txt\"")

assert response2.stdout.strip() == validation_message, f"\"{response2.stdout.strip()}\" != \"{validation_message}\"."
print(f"response2.stdout.strip(): {response2.stdout.strip()}")
sandbox2.terminate()
```

The expected output should be similar to this:
```log
Claim Name: sandbox-claim-bd10bdbc
response2.stdout.strip(): volume validation
```
