#!/bin/bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Exercises the sts-trust-verifier proxy against a live cluster:
#   1. Happy path: the real pod-identity-webhook-injected WebIdentityToken
#      verifies and the sandbox obtains credentials (existing check_irsa.py).
#   2. Negative path (unknown kid): a syntactically valid but
#      garbage-signature JWT with an unrecognized kid is rejected.
#   3. Negative path (real kid, forged signature): proves the RSA signature
#      check itself is enforced, not just kid lookup.
#   4. Negative path (wrong audience): a genuinely cluster-signed token
#      (via `kubectl create token`) with the wrong audience is rejected --
#      proves the audience check specifically, since cases 2-3 never reach
#      claim validation.
#   5. Negative path (mismatched issuer): a genuinely cluster-signed,
#      correct-audience token is rejected when the verifier is pointed at a
#      different EXPECTED_ISSUER -- proves the issuer check specifically.
#
# Assumes cert-manager and amazon-eks-pod-identity-webhook are already
# installed on the target cluster (see README.md prerequisites) -- this
# script does not install cluster-wide dependencies for you.

set -e
# Without pipefail, a failing `kubectl ... | python3 -c ...` (e.g. the issuer
# discovery and REAL_KID pipelines below) only fails the script if python3
# itself exits non-zero -- a kubectl error upstream in the pipe would
# otherwise be silently swallowed, since only the last command's exit status
# counts under plain set -e.
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

NS="irsa-sim-ns"
VERIFIER_URL="https://sts-trust-verifier.${NS}.svc.cluster.local:4566"

if ! kubectl get deployment pod-identity-webhook -n default >/dev/null 2>&1; then
    echo "amazon-eks-pod-identity-webhook not found in the 'default' namespace." >&2
    echo "Install it first -- see README.md Prerequisites." >&2
    exit 1
fi

cleanup() {
    echo "Cleaning up..."
    kubectl delete --ignore-not-found -f sandbox.yaml
    kubectl delete --ignore-not-found -f check-script-configmap.yaml
    kubectl delete --ignore-not-found -f trust-verifier.yaml
    kubectl delete --ignore-not-found -f trust-verifier-tls.yaml
    kubectl delete --ignore-not-found -f trust-verifier-configmap.yaml
    kubectl delete --ignore-not-found -f localstack.yaml
    kubectl delete --ignore-not-found -f localstack-tls.yaml
    kubectl delete --ignore-not-found -f serviceaccount.yaml
    kubectl delete --ignore-not-found -f namespace.yaml
}
trap cleanup EXIT

# Every python3 -c snippet below runs inside irsa-sim-sandbox and posts to
# VERIFIER_URL (https). They all need to trust sts-trust-verifier's
# cert-manager-issued cert, mounted into the sandbox pod at
# /irsa-sim-tls/ca.crt (see sandbox.yaml) -- this helper builds that once so
# each snippet below doesn't repeat the ssl.create_default_context() setup.
ssl_helper() {
    cat <<'PY'
import ssl
_ssl_ctx = ssl.create_default_context(cafile="/irsa-sim-tls/ca.crt")
PY
}

post_token() {
    local role_session_name="$1"
    local token="$2"
    kubectl -n "${NS}" exec irsa-sim-sandbox -- python3 -c "
$(ssl_helper)
import urllib.parse, urllib.request
body = urllib.parse.urlencode({
    'Action': 'AssumeRoleWithWebIdentity',
    'Version': '2011-06-15',
    'RoleArn': 'arn:aws:iam::000000000000:role/irsa-sim-role',
    'RoleSessionName': '${role_session_name}',
    'WebIdentityToken': '${token}',
}).encode()
req = urllib.request.Request('${VERIFIER_URL}', data=body, method='POST')
try:
    print(urllib.request.urlopen(req, context=_ssl_ctx).read().decode())
except urllib.error.HTTPError as e:
    print(e.read().decode())
"
}

# `kubectl rollout status` blocks until the control plane reports the new
# ReplicaSet fully up and the old one scaled down, but kube-proxy's local
# dataplane (iptables/ipvs) converges to match asynchronously -- there's a
# brief window, observed directly in practice, where a request made the
# instant rollout status returns can still land on connection state pointed
# at the just-terminated old pod. Retrying briefly absorbs that window
# without masking a real rejection failure (an actual bug fails every
# attempt, not just the first).
assert_rejected() {
    local role_session_name="$1"
    local token="$2"
    local failure_message="$3"
    local response
    for attempt in 1 2 3 4 5; do
        response=$(post_token "${role_session_name}" "${token}")
        if echo "${response}" | grep -q "InvalidIdentityToken"; then
            echo "${response}"
            return 0
        fi
        echo "attempt ${attempt}: not yet rejected, retrying in 2s..." >&2
        sleep 2
    done
    echo "${response}"
    echo "" >&2
    echo "${failure_message}" >&2
    exit 1
}

echo "Applying namespace, ServiceAccount..."
kubectl apply -f namespace.yaml
kubectl apply -f serviceaccount.yaml

echo "Issuing LocalStack's TLS certificate..."
kubectl apply -f localstack-tls.yaml
kubectl -n "${NS}" wait --for=condition=ready certificate/localstack-tls --timeout=60s

echo "Applying LocalStack..."
kubectl apply -f localstack.yaml
kubectl -n "${NS}" wait --for=condition=ready pod -l app=localstack --timeout=120s

echo "Discovering this cluster's real OIDC issuer..."
ISSUER=$(kubectl get --raw /.well-known/openid-configuration | python3 -c 'import json,sys; print(json.load(sys.stdin)["issuer"])')
echo "Issuer: ${ISSUER}"

echo "Issuing sts-trust-verifier's TLS certificate..."
kubectl apply -f trust-verifier-tls.yaml
kubectl -n "${NS}" wait --for=condition=ready certificate/sts-trust-verifier-tls --timeout=60s

echo "Applying sts-trust-verifier (pinned to the discovered issuer)..."
kubectl apply -f trust-verifier-configmap.yaml
kubectl apply -f trust-verifier.yaml
kubectl -n "${NS}" set env deployment/sts-trust-verifier "EXPECTED_ISSUER=${ISSUER}"
kubectl -n "${NS}" rollout status deployment/sts-trust-verifier --timeout=120s

echo "Applying check script and sandbox..."
kubectl apply -f check-script-configmap.yaml
kubectl apply -f sandbox.yaml
# 300s, not 120s: the sandbox image is a few hundred MB and a cold pull on a
# fresh kind node (no local cache) can comfortably exceed 120s.
kubectl -n "${NS}" wait --for=condition=Ready pod irsa-sim-sandbox --timeout=300s

echo "=== Happy path: real webhook-injected token ==="
kubectl -n "${NS}" exec irsa-sim-sandbox -- printenv AWS_ROLE_ARN AWS_WEB_IDENTITY_TOKEN_FILE >/dev/null
kubectl -n "${NS}" exec irsa-sim-sandbox -- sh -c 'HOME=/tmp python3 -m pip install --user --quiet "boto3>=1.29"'
kubectl -n "${NS}" exec irsa-sim-sandbox -- sh -c 'HOME=/tmp python3 /irsa-sim/check_irsa.py'
echo "Happy path OK."

echo "=== Negative path: forged token (unknown kid) must be rejected ==="
# Builds a syntactically valid JWT (three base64url segments) whose
# signature is garbage -- no crypto library needed, since any signature
# that doesn't match the cluster's real signing key must be rejected.
FORGED_TOKEN=$(kubectl -n "${NS}" exec irsa-sim-sandbox -- python3 -c '
import base64, json
def b64u(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64u(json.dumps({"alg": "RS256", "kid": "not-a-real-kid"}).encode())
payload = b64u(json.dumps({"iss": "https://kubernetes.default.svc.cluster.local", "aud": "sts.amazonaws.com", "exp": 9999999999, "iat": 0}).encode())
sig = b64u(b"not-a-real-signature")
print(f"{header}.{payload}.{sig}")
')

assert_rejected "forged-token-test" "${FORGED_TOKEN}" \
    "Forged token was NOT rejected -- trust-boundary verification is broken."
echo "Negative path OK (unknown kid): forged token correctly rejected."

echo "=== Negative path: real kid, forged signature, must still be rejected ==="
# The check above proves the kid-lookup rejects an unrecognized key, but
# doesn't exercise the actual RSA signature check. Reuse the real webhook
# token's own kid (a real key the verifier will find) with a bogus
# signature, so this only passes if the signature bytes are cryptographically
# verified, not merely if a matching kid is found in the JWKS.
REAL_KID=$(kubectl -n "${NS}" exec irsa-sim-sandbox -- sh -c 'cat "$AWS_WEB_IDENTITY_TOKEN_FILE"' | python3 -c "
import sys, base64, json
token = sys.stdin.read().strip()
header_b64 = token.split('.')[0]
header_b64 += '=' * (-len(header_b64) % 4)
print(json.loads(base64.urlsafe_b64decode(header_b64))['kid'])
")

FORGED_TOKEN_REAL_KID=$(kubectl -n "${NS}" exec irsa-sim-sandbox -- python3 -c "
import base64, json
def b64u(b):
    return base64.urlsafe_b64encode(b).rstrip(b'=').decode()
header = b64u(json.dumps({'alg': 'RS256', 'kid': '${REAL_KID}'}).encode())
payload = b64u(json.dumps({'iss': 'https://kubernetes.default.svc.cluster.local', 'aud': 'sts.amazonaws.com', 'exp': 9999999999, 'iat': 0}).encode())
sig = b64u(b'still-not-a-real-signature-bytes')
print(f'{header}.{payload}.{sig}')
")

assert_rejected "forged-real-kid-test" "${FORGED_TOKEN_REAL_KID}" \
    "Real-kid/forged-signature token was NOT rejected -- signature verification is broken."
echo "Negative path OK (real kid, forged signature): correctly rejected."

echo "=== Negative path: real signature, wrong audience, must be rejected ==="
# The two checks above never reach claim validation -- an unknown kid fails
# at JWKS lookup, and a forged signature fails at the signature check. A
# regression that silently dropped the audience/issuer checks in
# jwt.decode() would still pass both. This uses kubectl's own TokenRequest
# API to mint a token that is genuinely signed by the cluster's real key
# (same kid, same issuer) but with a deliberately wrong audience, so
# rejection here can only come from the audience check itself.
WRONG_AUDIENCE_TOKEN=$(kubectl create token irsa-sim-sa -n "${NS}" --audience="not-sts.amazonaws.com" --duration=10m)

assert_rejected "wrong-audience-test" "${WRONG_AUDIENCE_TOKEN}" \
    "Wrong-audience token was NOT rejected -- audience validation is broken."
echo "Negative path OK (real signature, wrong audience): correctly rejected."

echo "=== Negative path: real signature, mismatched EXPECTED_ISSUER, must be rejected ==="
# Same idea for the issuer check: mint a genuinely cluster-signed,
# correct-audience token, but point the verifier at a different
# EXPECTED_ISSUER than the one that actually signed it, so rejection here
# can only come from the issuer check.
CORRECT_AUDIENCE_TOKEN=$(kubectl create token irsa-sim-sa -n "${NS}" --audience="sts.amazonaws.com" --duration=10m)
kubectl -n "${NS}" set env deployment/sts-trust-verifier "EXPECTED_ISSUER=https://wrong-issuer.example.com"
kubectl -n "${NS}" rollout status deployment/sts-trust-verifier --timeout=120s

assert_rejected "wrong-issuer-test" "${CORRECT_AUDIENCE_TOKEN}" \
    "Token with a mismatched EXPECTED_ISSUER was NOT rejected -- issuer validation is broken."
echo "Negative path OK (real signature, mismatched issuer): correctly rejected."

echo ""
echo "Test finished."
