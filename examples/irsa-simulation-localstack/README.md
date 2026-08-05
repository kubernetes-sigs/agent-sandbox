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
automatically and exchanges the token for credentials via LocalStack's mocked
`AssumeRoleWithWebIdentity` — no application code changes needed.

**Important caveat:** LocalStack accepts the web identity token without
verifying its signature against a real OIDC issuer's JWKS. This proves the
pod correctly *discovers and uses* IRSA-style credentials — it does **not**
prove that a real AWS account would trust the token, or that an IRSA trust
policy is configured correctly. Treat this as a code-path smoke test, not a
security validation of the trust boundary.

## Files

- `namespace.yaml` — namespace for this example
- `serviceaccount.yaml` — ServiceAccount annotated with a (non-existent, mock)
  IAM role ARN
- `localstack.yaml` — LocalStack Deployment + Service, `SERVICES=sts` only
- `check-script-configmap.yaml` — the boto3 smoke-test script, mounted into
  the sandbox pod
- `sandbox.yaml` — a Sandbox using the annotated ServiceAccount, with
  `AWS_ENDPOINT_URL_STS` pointed at the in-cluster LocalStack Service

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

### 1. Apply the resources

```sh
kubectl apply -f namespace.yaml
kubectl apply -f serviceaccount.yaml
kubectl apply -f localstack.yaml
kubectl apply -f check-script-configmap.yaml
kubectl apply -f sandbox.yaml
```

### 2. Wait for both to be ready

```sh
kubectl -n irsa-sim-ns wait --for=condition=Ready pod -l app=localstack --timeout=120s
kubectl -n irsa-sim-ns get sandbox irsa-sim-sandbox
```

### 3. Confirm the webhook injected IRSA env vars

```sh
kubectl -n irsa-sim-ns exec irsa-sim-sandbox -- printenv AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE
```

### 4. Run the credential check

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
credentials and exchanged them via LocalStack's mocked STS — with no
application code aware that it isn't talking to real AWS.

## Cleanup

```sh
kubectl delete -f sandbox.yaml
kubectl delete -f check-script-configmap.yaml
kubectl delete -f localstack.yaml
kubectl delete -f serviceaccount.yaml
kubectl delete -f namespace.yaml
```

## Customization

- **Using a `SandboxTemplate` instead of a bare `Sandbox`:** the controller's
  auto-generated per-template `NetworkPolicy` restricts egress to the public
  internet and blocks other in-cluster services by default (see
  [`docs/security/threat_model.md`](../../docs/security/threat_model.md)).
  Reaching LocalStack from a `SandboxTemplate`-managed pod needs a
  supplemental, additive `NetworkPolicy` scoped to the LocalStack
  Service/namespace — additional `NetworkPolicy` objects selecting the same
  pods are unioned, not overridden.
- **Moving to real EKS:** swap the mock `eks.amazonaws.com/role-arn` for a
  real IAM role ARN, remove `AWS_ENDPOINT_URL_STS` so the SDK talks to real
  AWS STS, and register the cluster's actual OIDC provider with that role's
  trust policy.
