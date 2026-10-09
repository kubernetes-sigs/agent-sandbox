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
- Nodes able to pull `registry.k8s.io/e2e-test-images/agnhost:2.53`.

All resources are in `claimed-sandbox-service`. Use a nonempty, explicit test
context and a fresh namespace. Follow the checks below manually; this example
does not provide an automated verifier or automatic cleanup. Do not run cleanup
against a namespace containing other work.

## Deploy the warm reserve and group entry point

From this directory:

```bash
CONTEXT=your-test-context
NAMESPACE=claimed-sandbox-service

kubectl --context="$CONTEXT" get namespace "$NAMESPACE" --ignore-not-found
```

Proceed only if that command succeeds without showing an existing namespace.
If the namespace exists or the API request fails, stop before applying resources.

```bash
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

for request in $(seq 1 10); do
  printf 'Request %s: ' "$request"
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
Compare the returned hostnames with the claimed Pod names. The ten requests are
only a sample, not a guaranteed split: repeat after forwarding converges if
only one backend was observed, and investigate any unclaimed hostname.

The request path is caller -> Service -> application port 8000. It does **not**
pass through sandbox-router and needs no `X-Sandbox-ID`. This is not a shared
endpoint for sandboxd's per-Sandbox process or filesystem management sessions.

## Readiness

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
```

Once forwarding converges, fresh requests from the previous section should reach
only the other ready claimed Pod. Restore the selected Pod:

```bash
kubectl --context="$CONTEXT" -n "$NAMESPACE" exec "$POD" -c app -- \
  rm /tmp/not-ready
kubectl --context="$CONTEXT" -n "$NAMESPACE" wait \
  --for=condition=Ready "pod/$POD" --timeout=60s
kubectl --context="$CONTEXT" -n "$NAMESPACE" get endpointslices \
  -l kubernetes.io/service-name=claimed-sandbox-entry -o yaml
```

An unready endpoint may still appear in an EndpointSlice with `ready: false`;
inspect conditions, not only the address list. Readiness comes from the Pod,
not directly from SandboxClaim status. Do not enable
`publishNotReadyAddresses` for readiness-gated application serving.

After restoration, both claimed Pods should again be eligible endpoints.

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

## Check allowed and denied access

Compare both callers against the same Service IP so DNS failures cannot be
mistaken for policy denial. For IPv6 addresses, enclose the address in brackets
in `TARGET_URL`, for example `http://[$SERVICE_IP]:80/hostname`.

```bash
SERVICE_IP="$(kubectl --context="$CONTEXT" -n "$NAMESPACE" get service \
  claimed-sandbox-entry -o jsonpath='{.spec.clusterIP}')"
TARGET_URL="http://$SERVICE_IP:80/hostname"

kubectl --context="$CONTEXT" -n "$NAMESPACE" exec allowed-client -- \
  curl -fsS --connect-timeout 2 --max-time 5 "$TARGET_URL"
kubectl --context="$CONTEXT" -n "$NAMESPACE" exec denied-client -- \
  curl -fsS --connect-timeout 2 --max-time 5 "$TARGET_URL"
echo "Exit code: $?"
```

The allowed request must succeed and return a claimed Pod hostname; otherwise
stop and diagnose it before interpreting the denied result. The denied request
is expected to time out with exit code `28` for this policy; a successful response
means isolation is not working. Other failures, such as an exec error, are not
proof of denial.

Repeat those two requests against the restored claimed Pod's application port:

```bash
POD_IP="$(kubectl --context="$CONTEXT" -n "$NAMESPACE" get pod "$POD" \
  -o jsonpath='{.status.podIP}')"
TARGET_URL="http://$POD_IP:8000/hostname"
```

Curl's limits bound the in-Pod HTTP request, not the entire kubectl exec stream;
interrupt a stalled exec manually and check the cluster before continuing.

## Remove a member

Delete one Claim, then inspect the remaining serving Pods and endpoints:

```bash
kubectl --context="$CONTEXT" -n "$NAMESPACE" delete sandboxclaim serving-a --wait=false
kubectl --context="$CONTEXT" -n "$NAMESPACE" get pods \
  -l sandbox.users.io/serving-group=demo
kubectl --context="$CONTEXT" -n "$NAMESPACE" get endpointslices \
  -l kubernetes.io/service-name=claimed-sandbox-entry -o yaml
```

Deleting `serving-a` removes its owned Sandbox. After reconciliation and
forwarding convergence, the Service retains only `serving-b`; its caller-facing
address stays the same. Repeat the fresh-connection requests to check continued
serving by that remaining Pod. The pool replenishes its reserve, but reserve Pods
remain outside the group. Endpoint updates are asynchronous, and existing
connections are not guaranteed to migrate or drain without application handling.

## Cleanup

After confirming the explicit context and that the namespace contains only this
example's resources, remove it. Cleanup is manual, including after a failed check:

```bash
kubectl --context="$CONTEXT" delete namespace "$NAMESPACE" --wait=false
```

Related issue: [Routing requests across multiple claimed Sandboxes](https://github.com/kubernetes-sigs/agent-sandbox/issues/1615).
