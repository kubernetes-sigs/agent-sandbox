# Route application traffic across claimed Sandboxes

Use a normal Kubernetes Service to reach a group of already claimed Sandboxes
without choosing a Sandbox ID for every application request. This example uses
existing APIs; it does not add a group-routing mode to sandbox-router.

```text
allowed-client -> claimed-sandbox-entry (ClusterIP, port 80)
                    +-> claimed Sandbox A / application:8000
                    +-> claimed Sandbox B / application:8000

Unclaimed warm-pool Pods have no serving-group label and are not backends.
```

Each application returns its Pod hostname. The Kubernetes `agnhost` image is
configured to override all HTTP paths with `/hostname`, so its shell and upload
handlers are not exposed. This is a disposable demonstration workload, not a
production application server. No SDK, sandboxd, Gateway controller, image build,
or persistent storage is needed for the example itself.

## Prerequisites

- A Kubernetes cluster with the Agent Sandbox controller **and extensions**
  installed. See the [development guide](../../docs/development.md).
- A CNI that enforces Kubernetes NetworkPolicy. The default kind CNI does not;
  install a supported implementation, such as
  [Cilium on kind](https://docs.cilium.io/en/stable/installation/kind/), before
  using the negative access checks.
- `kubectl`, Bash, and permission to manage this example's dedicated namespace.
- Python 3 on the local machine for automated verification (standard library
  only; no extra packages).
- Nodes able to pull `registry.k8s.io/e2e-test-images/agnhost:2.53`.

All resources are in `claimed-sandbox-service`. Choose an explicit test context
and use a fresh namespace; do not run the cleanup command against a namespace
containing other work.

## Deploy the warm reserve and group entry point

From this directory:

```bash
CONTEXT=your-test-context
NAMESPACE=claimed-sandbox-service

kubectl --context="$CONTEXT" apply -k .
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait \
  --for=jsonpath='{.status.readyReplicas}'=2 \
  sandboxwarmpool/serving-pool --timeout=180s
kubectl --context="$CONTEXT" -n "$NAMESPACE" get pods
kubectl --context="$CONTEXT" -n "$NAMESPACE" get endpointslices \
  -l kubernetes.io/service-name=claimed-sandbox-entry -o yaml
```

There are two ready reserve Sandboxes, but no eligible Service endpoints yet.
The Service selects `sandbox.users.io/serving-group: demo`. That label is absent
from `template.yaml` and is added only by the Claims below. Setting it on the
Template would also admit unclaimed warm-pool Pods.

## Claim two instances and send application requests

```bash
kubectl --context="$CONTEXT" -n "$NAMESPACE" apply -f claims.yaml
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait --for=condition=Ready \
  sandboxclaim/serving-a sandboxclaim/serving-b --timeout=180s
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait --for=condition=Ready \
  pod/allowed-client pod/denied-client --timeout=180s

kubectl --context="$CONTEXT" -n "$NAMESPACE" get sandboxclaims
kubectl --context="$CONTEXT" -n "$NAMESPACE" get pods \
  -l sandbox.users.io/serving-group=demo
kubectl --context="$CONTEXT" -n "$NAMESPACE" get endpointslices \
  -l kubernetes.io/service-name=claimed-sandbox-entry -o yaml

for i in $(seq 1 10); do
  kubectl --context="$CONTEXT" -n "$NAMESPACE" exec allowed-client -- \
    curl -fsS --connect-timeout 2 --max-time 5 -H 'Connection: close' \
    http://claimed-sandbox-entry/hostname
  printf '\n'
done
```

After reconciliation, the eligible endpoints are the two claimed Pods. The pool
replenishes its reserve, but replacement reserve Pods do not inherit the
Claim-only label. Claim metadata labels use the controller's default permitted
domain, `sandbox.users.io`; no label-domain configuration change is necessary.

The request path is caller -> Service -> application port 8000. It does **not**
pass through sandbox-router and needs no `X-Sandbox-ID`. This is not a shared
endpoint for sandboxd's per-Sandbox process or filesystem management sessions.

## Readiness and member removal

The startup probe checks the HTTP listener. The readiness probe checks the
listener and a local `/tmp/not-ready` marker, giving the demo a deterministic
way to take one application out of service while keeping its Pod running.

For a Pod name shown by the label query above:

```bash
POD=one-of-the-claimed-pod-names
kubectl --context="$CONTEXT" -n "$NAMESPACE" exec "$POD" -c app -- \
  touch /tmp/not-ready
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait \
  --for=condition=Ready=false "pod/$POD" --timeout=60s
kubectl --context="$CONTEXT" -n "$NAMESPACE" get endpointslices \
  -l kubernetes.io/service-name=claimed-sandbox-entry -o yaml

kubectl --context="$CONTEXT" -n "$NAMESPACE" exec "$POD" -c app -- \
  rm /tmp/not-ready
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait \
  --for=condition=Ready "pod/$POD" --timeout=60s
```

An unready endpoint may still appear in an EndpointSlice with `ready: false`;
inspect conditions, not only the address list. Readiness comes from the Pod,
not directly from SandboxClaim status. Do not enable
`publishNotReadyAddresses` for readiness-gated application serving.

Deleting `serving-a` removes its owned Sandbox. The Service eventually retains
only `serving-b`; its caller-facing address stays the same. Claim labels, Pod
readiness and endpoint updates converge asynchronously, and existing connections
are not guaranteed to migrate or drain without application-level handling.

## Access policy and limitations

The Template uses `networkPolicyManagement: Managed` with custom rules:

- Only Pods labelled `app.kubernetes.io/name: claimed-sandbox-client` **in this
  namespace** can enter port 8000. The namespace and Pod selectors are in the
  same peer, so both must match. The example's denied client has a different
  label.
- Custom `spec.networkPolicy.ingress` and `.egress` replace the default managed
  rules; they are not appended to the router-only default. This application
  needs no outgoing connections, so egress is deliberately empty. Allowed
  response traffic is implicit. Add explicit dependencies such as DNS, model
  APIs, or router ingress if your real application needs them; other policies
  can add permissions.
- If callers are themselves egress-isolated, separately permit application
  traffic and DNS as needed. A destination ingress rule does not open source
  egress.
- Managed policy applies to all Sandboxes of this Template, including the warm
  reserve. Excluding an unclaimed Pod from a Service does not prevent a permitted
  caller from reaching that Pod directly by IP.

Service labels are membership, **not authentication or tenant isolation**.
This path bypasses sandbox-router's authorization; use a single trust domain
for this example, and provide separate application/Gateway authentication and
RBAC in a real deployment. The Service exposes only the application port, not
sandboxd management ports.

Instances must be interchangeable for these requests. A Service does not promise
strict per-HTTP-request round robin: keep-alive, connection pools and HTTP/2 can
reuse a backend connection. `ClientIP` affinity is not conversation-ID affinity
and does not preserve a replaced Pod's local state. Share conversational state
at the application layer or implement explicit session routing when necessary;
do not blindly retry partially completed writes on another Sandbox.

The per-Sandbox headless Service is a different resource; this shared entry is
an ordinary ClusterIP Service. For an external HTTP entry point, an optional
Gateway/HTTPRoute can reference this Service as its backend. Configure ingress
for the Gateway's actual data-plane peers and use a compatible Gateway controller;
Gateway installation and configuration are outside this example.

## Automated verification and cleanup

On a test cluster, **before** deploying the example manually:

```bash
bash verify.sh "$CONTEXT"
```

The verifier refuses an existing example namespace, creates its own resources,
and checks warm-only exclusion, two claimed backends, requests reaching both
backends, readiness removal/restoration, allowed versus denied access through
both Service and Pod IP, and Claim deletion with continued serving. It cleans
up only the namespace it created after checking its UID. An unenforced policy
fails verification instead of silently skipping the denial checks.

Every `kubectl exec` in the verifier has a local 15-second process deadline in
addition to kubectl's API and curl's network timeouts. A stalled exec stream
fails verification and terminates its local process group so cleanup can run;
it does not count as a successful NetworkPolicy denial. Python 3 is a local
verification dependency only, not a dependency of the application image.

To run offline verifier regressions with Python's standard library (no cluster
or Python packages required):

```bash
python3 test_verify.py
```

These intercept kubectl and accelerate polling to check argument safety,
initial forwarding convergence, bounded failures (including stalled exec), and
cleanup. They do not replace the functional verification against an enforcing CNI.

To remove a manually deployed example, after confirming the context and namespace:

```bash
kubectl --context="$CONTEXT" delete namespace "$NAMESPACE"
```

Related issue: [Routing requests across multiple claimed Sandboxes](https://github.com/kubernetes-sigs/agent-sandbox/issues/1615).
