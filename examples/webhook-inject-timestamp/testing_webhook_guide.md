# Guide: Creating a Mutating Admission Webhook

This guide outlines the steps to create, deploy, and configure a simple Mutating Admission Webhook
in a Kubernetes cluster.

## Purpose: Why use a Webhook?

To accurately measure the `ClaimStartupLatency` of a `SandboxClaim`, we need to know the exact time
in milliseconds the creation request was received by the Kubernetes API server.

### Limitations of Default Kubernetes Timestamps

You might wonder why we don't just use the standard `creationTimestamp` field in the resource's
metadata. The reason is **precision**:

- The default `creationTimestamp` in Kubernetes is truncated to **seconds**.
- For accurate latency measurements (which are often in milliseconds), this precision is insufficient.
- This webhook adds a custom annotation (`agents.x-k8s.io/webhook-first-observed-at`) with a
  **nanosecond-precision** timestamp.

### Webhook vs Controller Metrics

By default, the `agent-sandbox-controller` records the time it **first observes** (the time when the
controller's **informer cache** receives the watch event for the newly created `SandboxClaim`) the
claim.

This can be delayed by:

- API server latency.
- Controller queueing delays.

By using both the webhook timestamp and the controller's observation time, we can track two distinct
metrics:

1.  **Controller-observed Latency**: Time from controller observation to resource ready.
2.  **End-to-End Latency**: Time from webhook admission (creation) to resource ready.

This separation helps us identify if startup delays are due to controller processing or external
factors (like API server queueing).

### Caveats on Timestamp Skew

> [!WARNING]
> While this approach provides higher precision, it introduces a dependency on clock synchronization.
> If the node running the webhook server and the node running the controller have **clock skew**,
> the comparison of timestamps may not be perfectly accurate. In managed environments like GKE,
> nodes are typically synchronized via NTP, minimizing this risk, but it is still a factor to
> consider in distributed systems.

## Prerequisites

- A Kubernetes cluster with agent-sandbox installed with extensions enabled.
- `kubectl` configured to access the cluster.
- Docker installed.

---

## Step 1: Webhook Server Implementation

Create a Go server that handles `AdmissionReview` requests.

### `main.go`

Refer to the full implementation in [main.go](./main.go).

---

## Step 2: Build and Load/Push the Webhook Image

You can build the webhook server using the provided `Dockerfile`.

### Build Image

Navigate to the webhook directory:

```bash
cd examples/webhook-inject-timestamp
```

Set the image tag and build the image:

```bash
# Choose ONE of the following:
# For GKE, set "us-docker.pkg.dev/<project-id>/<repo-name>/" to your Artifact Registry path
export IMAGE_TAG="us-docker.pkg.dev/<project-id>/<repo-name>/webhook-image:latest"

# OR for Kind:
# export IMAGE_TAG="kind.local/webhook-image:latest"

docker build -t ${IMAGE_TAG} .
```

Then either push or load the image depending on your cluster:

```bash
# For GKE
docker push ${IMAGE_TAG}

# For Kind
kind load docker-image ${IMAGE_TAG} --name agent-sandbox
```

---

## Steps 3 & 4: TLS Certificates with cert-manager

Webhooks should be served over HTTPS with a valid certificate. `cert-manager` can be used to
automatically handle certificate generation and injection.

### Step 3: Install cert-manager

If not already installed, install `cert-manager` via official manifests:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
```

Verify pods are running:

```bash
kubectl get pods -n cert-manager
```

> [!NOTE]
> **GKE Autopilot Compatibility**:
> The default `cert-manager` manifest attempts to use the `kube-system` namespace for leader
> election, which is forbidden on GKE Autopilot. If you are using GKE Autopilot, you will see
> permission errors in the logs of `cert-manager-controller` and `cert-manager-cainjector`.
>
> To avoid this, you can modify the manifest on the fly using `sed` before applying it instead of
> using the command in Step 3:
>
> ```bash
> curl -sL https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml | \
> sed 's/--leader-election-namespace=kube-system/--leader-election-namespace=cert-manager/g' | \
> kubectl apply -f -
> ```
>
> And apply the provided role and binding in [cert-manager-rbac.yaml](./cert-manager-rbac.yaml)
> to grant lease permissions in the `cert-manager` namespace:
>
> ```bash
> kubectl apply -f cert-manager-rbac.yaml
> ```

### Step 4: Create Issuer and Certificate

Create a file `cert-manager-resources.yaml`:

Refer to the resources in [cert-manager-resources.yaml](./cert-manager-resources.yaml).

Apply it:

```bash
kubectl apply -f cert-manager-resources.yaml
```

---

## Step 5: Deployment Manifests

The Secret `webhook-certs` is automatically created and updated by `cert-manager`. You only need to
deploy the Webhook server and Service.

### Deployment and Service

Create `webhook-deployment.yaml`:

Refer to the deployment and service in [webhook-deployment.yaml](./webhook-deployment.yaml), and
update the image name `image: kind.local/webhook-image:latest` to match your image tag if not
using Kind.

Apply it:

```bash
kubectl apply -f webhook-deployment.yaml
```

---

## Step 6: MutatingWebhookConfiguration

This tells Kubernetes to send `SandboxClaim` creation requests to your webhook. We use
`cert-manager`'s CA injector to automatically populate the `caBundle`.

Create `mutating-webhook-configuration.yaml`:

Refer to the configuration in [mutating-webhook-configuration.yaml](./mutating-webhook-configuration.yaml).

Apply it:

```bash
kubectl apply -f mutating-webhook-configuration.yaml
```

> [!IMPORTANT]
> Since the Webhook server reads certificates on startup, if `cert-manager` updates the secret
> _after_ the pod starts, you may need to restart the deployment to pick up the new certificate:
>
> ```bash
> kubectl rollout restart deployment webhook-deployment -n agent-sandbox-system
> ```

---

## Step 7: Verification

To verify that the webhook is working correctly and adding the annotation, follow these steps:

### 1. Create a Test SandboxClaim

Create a file `test-claim.yaml`:

Refer to the example in [test-claim.yaml](./test-claim.yaml).

Apply it:

```bash
kubectl apply -f test-claim.yaml
```

### 2. Verify Annotation

Check the annotations of the created `SandboxClaim`:

```bash
kubectl get sandboxclaim test-webhook-claim -o yaml
```

You should see the `agents.x-k8s.io/webhook-first-observed-at` annotation with a timestamp:

```yaml
metadata:
  annotations:
    agents.x-k8s.io/webhook-first-observed-at: "2026-05-08T17:42:24.758063557Z"
```

If you also see `agents.x-k8s.io/controller-first-observed-at`, you can compare the timestamps to
confirm that the webhook observed the creation before the controller did.

> [!NOTE]
> This test claim references a non-existent warm pool (`nonexistent-pool`) and will not
> successfully start a sandbox (it will show status `WarmPoolNotFound`). This is expected for this
> test.

Delete the claim after confirming the annotation presence:

```bash
kubectl delete sandboxclaim test-webhook-claim
```
