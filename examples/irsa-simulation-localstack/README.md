# Simulating AWS IRSA locally with LocalStack

This example shows how to validate an AWS IRSA (IAM Roles for Service Accounts)
credential-loading code path against a sandbox pod, entirely on a local or
non-EKS cluster — no real AWS account, IAM role, or OIDC provider required.

## Overview

Real IRSA on Amazon EKS depends on the cluster's OIDC identity provider being
registered with AWS IAM, which only exists on real EKS clusters (and requires
IAM permissions many developers don't have on a shared/locked-down account).
That makes it hard to validate "does my sandboxed workload correctly pick up
and use IRSA-style credentials?" on a local kind/EKS Anywhere cluster, or in
CI.

This example combines two pieces that need no AWS IAM permissions at all:

1. **[`amazon-eks-pod-identity-webhook`](https://github.com/aws/amazon-eks-pod-identity-webhook)**
   (unmodified upstream project) — a mutating webhook that, for any pod whose
   ServiceAccount carries an `eks.amazonaws.com/role-arn` annotation, injects
   `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE` env vars and mounts a
   projected ServiceAccount token — identical to what real EKS does.
2. **[LocalStack](https://github.com/localstack/localstack)**, running
   in-cluster with only the `sts` service enabled, standing in for real AWS
   STS.

With both in place, the AWS SDK's default credential chain
(`WebIdentityRoleCredentialFetcher` in boto3) picks up the injected env vars
automatically and exchanges the token for credentials via a small
**`sts-trust-verifier`** proxy sitting in front of LocalStack — no
application code changes needed.

### Trust boundary verification

LocalStack's STS mock accepts any web identity token without checking it, so
on its own it only proves the pod *discovers and uses* IRSA-style
credentials — not that the token would be trusted by a real relying party.

`sts-trust-verifier` (`trust-verifier-configmap.yaml` /
`trust-verifier.yaml`) closes that gap for the one action this flow
actually uses (`AssumeRoleWithWebIdentity`): it fetches this cluster's own
JWKS from its `/openid/v1/jwks` discovery endpoint, verifies the token's
signature against it, and checks `aud` (`sts.amazonaws.com`, the webhook's
default), `iss`, and `exp` — the same cryptographic check a real AWS IAM
OIDC provider performs. Only on success does it forward the request to
LocalStack.

**Remaining caveat:** this still doesn't validate a *real* AWS account's IAM
OIDC provider registration or role trust policy — there is no real AWS
account involved. It proves the verification mechanics work correctly
against the cluster's actual signing key; it is not a substitute for
validating your real EKS trust policy configuration.

The proxy never derives the JWKS URL from the token being verified (that
would let a caller redirect it to an arbitrary URL — an SSRF vector); the
JWKS endpoint is a fixed, in-cluster constant, and the token's `iss` claim is
only ever compared against an operator-supplied `EXPECTED_ISSUER`, never
dereferenced.

**Key rotation:** a *known* `kid` is only re-checked against the cluster's
JWKS every `_JWKS_MAX_CACHE_AGE_SECONDS` (300s), not on every request. Without
this, a key removed from the cluster's JWKS (rotation, or revoking a
compromised key) would stay trusted by this proxy indefinitely, until it
happens to restart. A token using a since-rotated-out `kid` is rejected once
that window elapses, not never.

### Transport encryption

Both hops carrying the WebIdentityToken and the resulting AWS credentials
are TLS, and both are genuinely certificate-verified (neither uses
`verify=False`):

- **sandbox → sts-trust-verifier:** this is the hop that matters most —
  it's where the real WebIdentityToken goes out and the real (mocked)
  credentials come back. `trust-verifier-tls.yaml` issues a certificate via
  cert-manager's `selfsigned` `ClusterIssuer` (the one
  `amazon-eks-pod-identity-webhook`'s own install already creates), and the
  sandbox pod is configured to actually verify it via `AWS_CA_BUNDLE`
  (`sandbox.yaml`).
- **sts-trust-verifier → LocalStack:** LocalStack auto-detects TLS and
  serves HTTPS on its edge port with no extra config, but its baked-in
  certificate is a fixed, publicly-known test artifact shared by every
  LocalStack install (`CN=localhost`, self-signed by "LocalStack Org") —
  not something meaningful to verify against. `localstack-tls.yaml` instead
  issues LocalStack its own certificate matching its real Service DNS name
  (LocalStack reads this via `CUSTOM_SSL_CERT_PATH`, a single PEM file
  containing both the key and cert — `localstack.yaml`'s init container
  concatenates cert-manager's separate `tls.key`/`tls.crt` into that
  format), and `sts-trust-verifier` verifies against it via
  `LOCALSTACK_CA_FILE` instead of skipping verification.

### Availability

`sts-trust-verifier` uses `http.server.ThreadingHTTPServer`, which spawns one
thread per connection with no cap on the total number of threads.
`Handler.timeout` (30s) bounds how long a single slow or incomplete
connection can hold its thread — without it, a caller that opens a
connection and never finishes sending a request would block that thread
indefinitely, and enough of them exhausts an otherwise uncapped server.
There is still no hard ceiling on total concurrent threads; fine for this
single-sandbox demo, not something to copy as-is into anything handling
untrusted, high-volume traffic.

## Files

- `namespace.yaml` — namespace for this example
- `serviceaccount.yaml` — ServiceAccount annotated with a (non-existent, mock)
  IAM role ARN
- `localstack.yaml` — LocalStack Deployment + Service, `SERVICES=sts` only,
  with an init container giving it a cert-manager-issued TLS certificate
- `localstack-tls.yaml` — cert-manager `Certificate` for LocalStack, issued
  by the `selfsigned` `ClusterIssuer`
- `trust-verifier-tls.yaml` — cert-manager `Certificate` for
  `sts-trust-verifier`, issued by the `selfsigned` `ClusterIssuer`
- `trust-verifier-configmap.yaml` — the `sts-trust-verifier` proxy script
- `trust-verifier.yaml` — `sts-trust-verifier` Deployment + Service, sitting
  in front of LocalStack
- `check-script-configmap.yaml` — the boto3 smoke-test script, mounted into
  the sandbox pod
- `sandbox.yaml` — a Sandbox using the annotated ServiceAccount, with
  `AWS_ENDPOINT_URL_STS` pointed at the in-cluster `sts-trust-verifier`
  Service over HTTPS, and `AWS_CA_BUNDLE` set so it actually verifies that
  certificate
- `run-test-kind.sh` — automated test against a live cluster: happy path,
  forged-signature rejection, and wrong-audience/wrong-issuer rejection
  using real cluster-signed tokens

## Prerequisites

Install `amazon-eks-pod-identity-webhook` (real, unmodified upstream — not
vendored here):

```sh
# Pinned to the same release tag as the image below, not the moving `master`
# branch, so this doesn't break if upstream manifests change incompatibly.
kubectl apply -f https://raw.githubusercontent.com/aws/amazon-eks-pod-identity-webhook/v0.6.17/deploy/deployment-base.yaml
kubectl apply -f https://raw.githubusercontent.com/aws/amazon-eks-pod-identity-webhook/v0.6.17/deploy/auth.yaml
kubectl apply -f https://raw.githubusercontent.com/aws/amazon-eks-pod-identity-webhook/v0.6.17/deploy/service.yaml
kubectl apply -f https://raw.githubusercontent.com/aws/amazon-eks-pod-identity-webhook/v0.6.17/deploy/mutatingwebhook.yaml

# deployment-base.yaml ships with an unresolved IMAGE placeholder in the
# container spec — point it at a real released image tag (deploys into the
# `default` namespace; see deployment-base.yaml):
kubectl set image deployment/pod-identity-webhook -n default \
  pod-identity-webhook=public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.6.17
```

This requires `cert-manager` to already be installed on the cluster (used to
issue the webhook's TLS certificate).

## Usage

### 1. Apply LocalStack

```sh
kubectl apply -f namespace.yaml
kubectl apply -f serviceaccount.yaml
kubectl apply -f localstack-tls.yaml
kubectl -n irsa-sim-ns wait --for=condition=ready certificate/localstack-tls --timeout=60s
kubectl apply -f localstack.yaml
kubectl -n irsa-sim-ns wait --for=condition=ready pod -l app=localstack --timeout=120s
```

### 2. Issue `sts-trust-verifier`'s TLS certificate

```sh
kubectl apply -f trust-verifier-tls.yaml
kubectl -n irsa-sim-ns wait --for=condition=ready certificate/sts-trust-verifier-tls --timeout=60s
```

### 3. Point `sts-trust-verifier` at this cluster's real issuer

`EXPECTED_ISSUER` in `trust-verifier.yaml` is a placeholder — every cluster's
OIDC issuer is different, and the verifier must be pinned to the real one
(never derived from an incoming token; see "Trust boundary verification"
above):

```sh
ISSUER=$(kubectl get --raw /.well-known/openid-configuration | python3 -c 'import json,sys; print(json.load(sys.stdin)["issuer"])')
kubectl apply -f trust-verifier-configmap.yaml
kubectl apply -f trust-verifier.yaml
kubectl -n irsa-sim-ns set env deployment/sts-trust-verifier "EXPECTED_ISSUER=${ISSUER}"
# rollout status, not wait --for=condition=available: on a single-replica
# Deployment, `set env` surges a new pod before terminating the old one, so
# condition=available can pass while the Service still routes to the stale
# pod (still running with the old EXPECTED_ISSUER). rollout status blocks
# until the old ReplicaSet is fully scaled down.
kubectl -n irsa-sim-ns rollout status deployment/sts-trust-verifier --timeout=120s
```

### 4. Apply the sandbox

```sh
kubectl apply -f check-script-configmap.yaml
kubectl apply -f sandbox.yaml
kubectl -n irsa-sim-ns wait --for=condition=Ready pod irsa-sim-sandbox --timeout=120s
```

### 5. Confirm the webhook injected IRSA env vars

```sh
kubectl -n irsa-sim-ns exec irsa-sim-sandbox -- printenv AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE
```

### 6. Run the credential check

The base sandbox image doesn't ship `boto3`, so install it once inside the
pod before running the script (a real deployment would bake this into a
custom image layered on top of the base — see `examples/python-runtime-sandbox`).
The container runs as a non-root user whose home directory isn't writable, so
override `HOME` for the install and the script:

```sh
kubectl -n irsa-sim-ns exec irsa-sim-sandbox -- sh -c 'HOME=/tmp python3 -m pip install --user --quiet "boto3>=1.29"'
kubectl -n irsa-sim-ns exec irsa-sim-sandbox -- sh -c 'HOME=/tmp python3 /irsa-sim/check_irsa.py'
```

`python3 -m pip` (rather than a bare `pip`) guarantees the package installs
for the same interpreter that runs the script, regardless of what else is on
`PATH` in the container. The `boto3>=1.29` floor matters functionally, not
just stylistically: `AWS_ENDPOINT_URL_STS` is only honored automatically
starting around that botocore release — on an older version the check would
silently fall through to calling real AWS STS instead of LocalStack.

Expected output:

```
Credential provider: assume-role-with-web-identity
AccessKeyId: ASIA...
SecretKey present: True
SessionToken present: True
Assumed identity ARN: arn:aws:sts::000000000000:assumed-role/irsa-sim-role/botocore-session-...
```

This confirms the sandbox pod correctly discovered the webhook-injected
credentials, that `sts-trust-verifier` verified the token's signature,
issuer, and audience over a TLS connection the sandbox actually validated
(via `AWS_CA_BUNDLE`), and that the exchange succeeded via LocalStack's
mocked STS — with no application code aware that it isn't talking to real
AWS.

### 7. Demonstrate that a forged token is rejected

The check above only shows the happy path. To confirm the verification
actually has teeth, send a syntactically valid JWT with a garbage signature
straight to `sts-trust-verifier` and confirm it's rejected:

```sh
kubectl -n irsa-sim-ns exec irsa-sim-sandbox -- python3 -c '
import base64, json, ssl, urllib.parse, urllib.request, urllib.error

def b64u(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()

token = ".".join([
    b64u(json.dumps({"alg": "RS256", "kid": "not-a-real-kid"}).encode()),
    b64u(json.dumps({"iss": "https://kubernetes.default.svc.cluster.local", "aud": "sts.amazonaws.com", "exp": 9999999999, "iat": 0}).encode()),
    b64u(b"not-a-real-signature"),
])
body = urllib.parse.urlencode({
    "Action": "AssumeRoleWithWebIdentity",
    "Version": "2011-06-15",
    "RoleArn": "arn:aws:iam::000000000000:role/irsa-sim-role",
    "RoleSessionName": "forged-token-test",
    "WebIdentityToken": token,
}).encode()
# Same CA sandbox.yaml mounts for AWS_CA_BUNDLE -- verify the verifier's
# real cert-manager-issued cert rather than skip verification.
ssl_ctx = ssl.create_default_context(cafile="/irsa-sim-tls/ca.crt")
req = urllib.request.Request("https://sts-trust-verifier.irsa-sim-ns.svc.cluster.local:4566", data=body, method="POST")
try:
    print(urllib.request.urlopen(req, context=ssl_ctx).read().decode())
except urllib.error.HTTPError as e:
    print(e.read().decode())
'
```

Expected output contains `<Code>InvalidIdentityToken</Code>` — the request
is rejected before it ever reaches LocalStack. `run-test-kind.sh` automates
this, the happy path, and two more cases as a regression test: a
genuinely cluster-signed token with the wrong audience, and a genuinely
cluster-signed token checked against a deliberately mismatched
`EXPECTED_ISSUER`. Both use `kubectl create token` to mint a real,
correctly-signed token rather than a hand-built one, so they can only pass
if the audience/issuer checks themselves are enforced — the forged-token
cases above are rejected earlier (at JWKS lookup or signature
verification) and wouldn't catch a regression that dropped those two
checks specifically.

## Cleanup

```sh
kubectl delete -f sandbox.yaml
kubectl delete -f check-script-configmap.yaml
kubectl delete -f trust-verifier.yaml
kubectl delete -f trust-verifier-tls.yaml
kubectl delete -f trust-verifier-configmap.yaml
kubectl delete -f localstack.yaml
kubectl delete -f localstack-tls.yaml
kubectl delete -f serviceaccount.yaml
kubectl delete -f namespace.yaml
```

## Customization

- **Using a `SandboxTemplate` instead of a bare `Sandbox`:** the controller's
  auto-generated per-template `NetworkPolicy` restricts egress to the public
  internet and blocks other in-cluster services by default (see
  [`docs/security/threat_model.md`](../../docs/security/threat_model.md)).
  Reaching `sts-trust-verifier` (which the sandbox now talks to instead of
  LocalStack directly) from a `SandboxTemplate`-managed pod needs a
  supplemental, additive `NetworkPolicy` scoped to its Service/namespace —
  additional `NetworkPolicy` objects selecting the same pods are unioned, not
  overridden.
- **Moving to real EKS:** swap the mock `eks.amazonaws.com/role-arn` for a
  real IAM role ARN, remove `AWS_ENDPOINT_URL_STS` so the SDK talks to real
  AWS STS, and register the cluster's actual OIDC provider with that role's
  trust policy.
