# Native sandboxd gRPC through sandbox-router

This example uses sandboxd's existing generated Go client, not a new SDK connection
mode. No stub generation is needed. Sandbox creation remains a Kubernetes
control-plane operation; these calls reach an already-created Sandbox.

```text
Go client -- TLS / HTTP2 --> Envoy Gateway v1.9.2
          -- h2c --> Go sandbox-router:8080 -- h2c --> Sandbox / sandboxd:9090
```

The router forwards gRPC without decoding commands or registering ProcessService.
The same connection can address different Sandboxes through per-RPC metadata.
`Start` is server-streaming; stdin and signals are separate RPCs carrying the same
target. The router's metrics port 9090 is not sandboxd's port 9090. TLS protects
the first hop only; the internal h2c hops are not end-to-end encrypted.

## Prerequisites and isolated setup

Run from the repository root in Bash. You need the Go/toolchain version declared
by `go.mod`, Docker, kind, kubectl, Bash, and OpenSSL 3. The optional TLS lane also
needs Helm and installs **Envoy Gateway v1.9.2**, including compatible Gateway API
CRDs. Use a new disposable cluster: CRDs are cluster-scoped and must not replace
another controller's installation. For an existing cluster, first follow the
[provider-managed CRD instructions](https://gateway.envoyproxy.io/v1.9/install/install-helm/).
Do not use `make deploy-kind` here: it recreates its selected cluster.

The example uses ordinary containers, not a RuntimeClass. Change image references
if using a registry instead of kind. Credentials and private keys are stored in
a private temporary directory, never in the repository.

```sh
set -euo pipefail
umask 077
DEMO_CLUSTER=agent-sandbox-grpc-demo
CONTEXT=kind-agent-sandbox-grpc-demo
demo_dir="$(mktemp -d)"
export KUBECONFIG="$demo_dir/kubeconfig"
# This must be a new cluster; kind refuses an existing name. Do not recreate it.
kind create cluster --name "$DEMO_CLUSTER" --kubeconfig "$KUBECONFIG"
kubectl --context "$CONTEXT" cluster-info
dev/tools/push-images --image-prefix kind.local/ --image-tag grpc-router \
  --kind-cluster-name "$DEMO_CLUSTER" \
  --images agent-sandbox-controller sandbox-router-go sandboxd
# This tool uses the single-context KUBECONFIG above.
dev/tools/deploy-to-kube --image-prefix kind.local/ --image-tag grpc-router
kubectl --context "$CONTEXT" -n agent-sandbox-system rollout status \
  deployment/agent-sandbox-controller --timeout=180s
go build -o bin/sandbox-router-grpc ./examples/sandbox-router-grpc
openssl rand -hex 32 > "$demo_dir/router-key"
kubectl --context "$CONTEXT" apply -f examples/sandbox-router-grpc/workload.yaml
kubectl --context "$CONTEXT" -n grpc-router-demo create secret generic grpc-router-auth \
  --from-file=key="$demo_dir/router-key"
./bin/sandbox-router-grpc --mode mint-token --namespace grpc-router-demo \
  --sandbox box-a --secret-file "$demo_dir/router-key" --token-ttl 10m \
  > "$demo_dir/token"
kubectl --context "$CONTEXT" -n grpc-router-demo rollout status \
  deployment/sandbox-router-grpc --timeout=180s
kubectl --context "$CONTEXT" -n grpc-router-demo wait \
  --for=condition=Ready sandbox/box-a --timeout=180s
```

The existing scoped-token v1 authorizer binds this demo token to namespace/name
and rejects UID/Pod-IP overrides. It does **not** bind port, method or incarnation;
for production use the existing scoped-token v2 cache-backed contract when those
restrictions are required. Only a trusted issuer should have the signing secret.
The client reads a token file and sends `authorization: Bearer …` on every RPC;
the router consumes it and does not forward it to sandboxd. Never publish an
allow-all router as an authenticated public endpoint. Renew the token if it expires.

The Sandbox explicitly enables its Service, so the DNS-only router resolves
`box-a.grpc-router-demo.svc.cluster.local:9090`. For warm-pool Sandboxes without
Services, enable the existing router Pod-IP cache and its RBAC instead.

## Trusted internal h2c path

In another terminal, with the same private kubeconfig, forward **the router**, not
the Sandbox Pod. Check that local port 18080 is free first:

```sh
kubectl --context "$CONTEXT" -n grpc-router-demo port-forward \
  deployment/sandbox-router-grpc 18080:8080
```

Back in the setup terminal:

```sh
internal=(--address 127.0.0.1:18080 --plaintext --namespace grpc-router-demo \
  --sandbox box-a --port 9090 --timeout 30s --token-file "$demo_dir/token")
./bin/sandbox-router-grpc "${internal[@]}"
# stdout: hello through router; stderr: exit_code=0
./bin/sandbox-router-grpc "${internal[@]}" --mode start -- /bin/sh -c \
  'printf "first\n"; sleep 2; printf "last\n"'
# first is delivered before last and before process exit.
./bin/sandbox-router-grpc "${internal[@]}" --mode interact --stdin router
# stdout: ready, then input:router; stderr includes process_id and exit_code=0.
if ./bin/sandbox-router-grpc "${internal[@]}" -- /bin/sh -c \
  'printf hello; printf warning >&2; exit 7'; then
  echo 'unexpected zero exit' >&2
else
  test "$?" -eq 7
fi
```

`--mode signal` starts a long-lived process, receives output, sends SIGTERM through
another RPC, and prints its nonzero process exit. Use Ctrl-C on a long `--mode
start` call to cancel its RPC. Cancellation does not roll back side effects;
sandboxd owns process termination semantics. The positive `--timeout` bounds the
whole example session, including follow-up stdin/signal RPCs.

The example always sends `x-sandbox-id`, `x-sandbox-namespace`, and an explicit
`x-sandbox-port`. The router defaults the namespace to `default` if another client
omits it, but native gRPC has **no default target port**. Plaintext is only
appropriate inside a trusted network or this loopback port-forward.

## Separate TLS Gateway lane

Install only in the disposable cluster above. The standard chart installs shared
CRDs; do not run this against an unreviewed existing Gateway API installation. The
[versioned guide](https://gateway.envoyproxy.io/v1.9/install/install-helm/) describes
provider-managed alternatives.

```sh
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.9.2 \
  --kube-context "$CONTEXT" -n envoy-gateway-system --create-namespace
kubectl --context "$CONTEXT" -n envoy-gateway-system wait \
  --for=condition=Available deployment/envoy-gateway --timeout=300s
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=grpc-router-demo-ca \
  -addext basicConstraints=critical,CA:TRUE \
  -keyout "$demo_dir/ca.key" -out "$demo_dir/ca.crt"
openssl req -new -newkey rsa:2048 -nodes -subj /CN=grpc-router.local \
  -addext subjectAltName=DNS:grpc-router.local -addext extendedKeyUsage=serverAuth \
  -keyout "$demo_dir/tls.key" -out "$demo_dir/tls.csr"
openssl x509 -req -in "$demo_dir/tls.csr" -CA "$demo_dir/ca.crt" \
  -CAkey "$demo_dir/ca.key" -CAcreateserial -days 1 -copy_extensions copy \
  -out "$demo_dir/tls.crt"
kubectl --context "$CONTEXT" -n grpc-router-demo create secret tls grpc-router-tls \
  --cert="$demo_dir/tls.crt" --key="$demo_dir/tls.key"
kubectl --context "$CONTEXT" apply -f examples/sandbox-router-grpc/gateway.yaml
kubectl --context "$CONTEXT" -n grpc-router-demo wait \
  --for=condition=Programmed gateway/grpc-router --timeout=180s
kubectl --context "$CONTEXT" -n grpc-router-demo get grpcroute sandbox-router -o yaml
kubectl --context "$CONTEXT" -n grpc-router-demo get backendtrafficpolicy sandbox-router-grpc -o yaml
```

Check `Accepted=True` / `ResolvedRefs=True` on the route and acceptance of the
policy. `gateway.yaml` uses `timeout.http.requestTimeout`, `maxStreamDuration`, and
`streamIdleTimeout`, all `0s`, following the
[v1.9 timeout API](https://gateway.envoyproxy.io/v1.9/tasks/traffic/grpc-timeouts/).
These disable hidden Gateway limits; the client still supplies a finite deadline.
There is no Gateway retry or mirror policy. The separate Service's
`appProtocol: kubernetes.io/h2c` reaches the existing router port 8080 without
changing legacy `sandbox-router-svc`. The GRPCRoute has no service/method filter;
the router, not the Gateway, selects the Sandbox on each call.

Get the Envoy Service generated for **this** Gateway, then forward it in another
terminal using the same private kubeconfig; check local port 18443 is free:

```sh
envoy_service="$(kubectl --context "$CONTEXT" -n envoy-gateway-system get service \
  -l gateway.envoyproxy.io/owning-gateway-namespace=grpc-router-demo,gateway.envoyproxy.io/owning-gateway-name=grpc-router \
  -o jsonpath='{.items[0].metadata.name}')"
test -n "$envoy_service"
kubectl --context "$CONTEXT" -n envoy-gateway-system port-forward \
  "service/$envoy_service" 18443:443
```

In the setup terminal, run the explicit data-plane lane (about 75 seconds):

```sh
ADDRESS=127.0.0.1:18443 CA_FILE="$demo_dir/ca.crt" TOKEN_FILE="$demo_dir/token" \
  bash examples/sandbox-router-grpc/verify-gateway.sh
```

The client verifies the CA **and** `grpc-router.local` as the TLS server name/SNI;
there is no skip-verify option. The lane checks stdout/stderr/nonzero exit,
streaming, stdin, signal, a 20-second unary and 45-second stream, deadline,
cancellation, native routing/auth/backend errors, and wrong CA/server-name refusal.
Ready/Accepted alone are not data-plane results. This lane is separate from the
default e2e suite and its Python/cloud-provider-kind fixtures; missing Envoy must
not silently count as a pass.

## Network policy and verification

The existing [router policy](../../sandbox-router/deploy/networkpolicy.yaml) keeps
legacy TCP 8888 and adds TCP 9090 only to labelled sandboxd Pods in
`grpc-router-demo`. Adjust its namespace and router Pod selector when applying it
to this separate deployment. Preserve DNS, required apiserver access for
cache/TokenReview, and telemetry egress. Neither TLS nor a YAML file proves tenant
isolation: use an enforcing CNI and validate allowed **and** forbidden destinations.
Default kind networking does not demonstrate enforcement. Kata and gVisor are not
validated by this ordinary-container example.

Non-cluster protocol, real sandboxd, and example checks:

```sh
go test -race -tags=integration ./sandbox-router/... ./examples/sandbox-router-grpc
```

The ordinary-container e2e test forwards the router port and leaves direct
sandboxd tests intact. It creates and cleans its own namespace and needs no Envoy.
Use the single-context kubeconfig so the framework and kubectl reach the same
explicitly selected cluster:

```sh
IMAGE_PREFIX=kind.local/ IMAGE_TAG=grpc-router \
  go test -v ./test/e2e/extensions -run '^TestRunSandboxdViaGoRouter$' -count=1
```

## Troubleshooting and cleanup

`InvalidArgument` usually means invalid routing metadata, especially the port.
`Unauthenticated` means a missing, expired or invalid token; `PermissionDenied`
means the token targets another Sandbox or uses a forbidden override.
`Unavailable` points to Service DNS, readiness, the target port, or egress policy.
Check Sandbox readiness, router logs, route/policy conditions, and the h2c Service
hint. A certificate error requires correct CA/SAN/SNI, not disabled verification.
A nonzero `exit_code` is a command result, not an RPC error.

Access logs/traces include the final gRPC code (`unknown` when missing or invalid);
existing HTTP metrics still measure transport status, not RPC success. Do not log
tokens, private keys, command payloads or unredacted diagnostics.

Stop both port-forwards with Ctrl-C first. Remove only the resources and disposable
cluster created above; do not delete shared CRDs in an existing cluster:

```sh
kubectl --context "$CONTEXT" delete -f examples/sandbox-router-grpc/gateway.yaml
kubectl --context "$CONTEXT" delete namespace grpc-router-demo
helm uninstall eg --kube-context "$CONTEXT" -n envoy-gateway-system
kind delete cluster --name "$DEMO_CLUSTER"
rm -f "$demo_dir/router-key" "$demo_dir/token" "$demo_dir/kubeconfig" \
  "$demo_dir/ca.key" "$demo_dir/ca.crt" "$demo_dir/ca.srl" \
  "$demo_dir/tls.key" "$demo_dir/tls.csr" "$demo_dir/tls.crt"
rmdir "$demo_dir"
```

If you ran only the internal lane, omit Gateway/Helm cleanup. Local image removal
is optional: first check no other task uses the exact `:grpc-router` images. Do not
prune global Docker or Go caches as part of this example.
