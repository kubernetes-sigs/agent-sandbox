#!/usr/bin/env bash
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
# Self-contained end-to-end demo of agent-sandbox + dra.agents.net (HostTun
# default, zero sidecar, zero Pod privileges) + mandatory mTLS connect-proxy PEP.
#
# Usage:
#   # Build dra-agents-net and connect-proxy from a local agents.net checkout
#   # and run on a disposable kind v1.36 cluster:
#   AGENTS_NET_DIR=/path/to/agents.net ./demo.sh
#
#   # Or use published images against an existing Kubernetes 1.36+ cluster with
#   # NRI enabled and agent-sandbox already installed:
#   USE_EXISTING_CLUSTER=true KUBE_CONTEXT=<ctx> ./demo.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/manifests"
NS="agents-net-demo"

USE_EXISTING_CLUSTER="${USE_EXISTING_CLUSTER:-false}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox-agents-net-demo}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.36.1}"
PYTHON_IMAGE="${PYTHON_IMAGE:-python:3.14-slim}"
AGENTS_NET_DIR="${AGENTS_NET_DIR:-}"
DRIVER_IMAGE="${DRIVER_IMAGE:-ghcr.io/aojea/agents.net/dra-agents-net:latest}"
PEP_IMAGE="${PEP_IMAGE:-kind.local/agents-net-connect-proxy:demo}"
INSTALL_YAML_URL="${INSTALL_YAML_URL:-https://raw.githubusercontent.com/aojea/agents.net/main/kubernetes/install.yaml}"
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-latest}"
AGENT_SANDBOX_MANIFEST="${AGENT_SANDBOX_MANIFEST:-}"
SKIP_AGENT_SANDBOX_INSTALL="${SKIP_AGENT_SANDBOX_INSTALL:-false}"

TMPDIR="$(mktemp -d)"
CREATED_CLUSTER="false"

KUBECTL_ARGS=()
if [ -n "${KUBECONFIG:-}" ]; then
  KUBECTL_ARGS+=(--kubeconfig "${KUBECONFIG}")
fi
if [ -n "${KUBE_CONTEXT:-}" ]; then
  KUBECTL_ARGS+=(--context "${KUBE_CONTEXT}")
fi
k() {
  kubectl "${KUBECTL_ARGS[@]}" "$@"
}

cleanup() {
  local rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo "=== Demo failed (rc=${rc}); dumping diagnostics ===" >&2
    k -n kube-system logs -l app.kubernetes.io/name=dra-agents-net --tail=60 >&2 || true
    k -n agent-sandbox-system logs -l app=agent-sandbox-controller --tail=40 >&2 || true
    k -n "${NS}" get sandbox,sandboxwarmpool,sandboxclaim,resourceclaim,pod -o wide >&2 || true
    k -n "${NS}" logs deploy/connect-proxy --tail=40 >&2 || true
  fi
  if [ "${CREATED_CLUSTER}" = "true" ] && [ "${KEEP_CLUSTER}" != "true" ]; then
    kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi
  rm -rf "${TMPDIR}"
  exit "${rc}"
}
trap cleanup EXIT

if [ -n "${AGENTS_NET_DIR}" ]; then
  echo "=== 1. Build dra-agents-net and connect-proxy images from ${AGENTS_NET_DIR} ==="
  DRIVER_IMAGE="kind.local/dra-agents-net:demo"
  PEP_IMAGE="kind.local/agents-net-connect-proxy:demo"
  docker build --load -t "${DRIVER_IMAGE}" -f "${AGENTS_NET_DIR}/kubernetes/Dockerfile" "${AGENTS_NET_DIR}"
  docker build --load -t "${PEP_IMAGE}" -f "${AGENTS_NET_DIR}/kubernetes/test/Dockerfile.connect-proxy" "${AGENTS_NET_DIR}/sdk"
fi

if [ "${USE_EXISTING_CLUSTER}" != "true" ]; then
  echo "=== 2. Create disposable kind cluster (${KIND_CLUSTER_NAME}, ${KIND_NODE_IMAGE}) with NRI enabled ==="
  cat > "${TMPDIR}/kind-config.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
containerdConfigPatches:
  - |-
    [plugins."io.containerd.nri.v1.nri"]
      disable = false
EOF
  KUBECONFIG="${TMPDIR}/kubeconfig"
  KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
  KUBECTL_ARGS=(--kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}")
  kind create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --image "${KIND_NODE_IMAGE}" \
    --config "${TMPDIR}/kind-config.yaml" \
    --kubeconfig "${KUBECONFIG}" \
    --wait 120s
  CREATED_CLUSTER="true"
else
  echo "=== 2. Using existing cluster ==="
fi

if [ -n "${AGENTS_NET_DIR}" ]; then
  echo "=== 3. Load local images into kind cluster ==="
  kind load docker-image --name "${KIND_CLUSTER_NAME}" "${DRIVER_IMAGE}" "${PEP_IMAGE}"
fi

if [ "${SKIP_AGENT_SANDBOX_INSTALL}" != "true" ]; then
  echo "=== 4. Install agent-sandbox (core + extensions) ==="
  if [ -n "${AGENT_SANDBOX_MANIFEST}" ]; then
    k apply --server-side -f "${AGENT_SANDBOX_MANIFEST}"
  else
    version="${AGENT_SANDBOX_VERSION}"
    if [ "${version}" = "latest" ]; then
      version="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
        https://github.com/kubernetes-sigs/agent-sandbox/releases/latest | sed 's#.*/tag/##')"
    fi
    k apply --server-side -f \
      "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${version}/sandbox-with-extensions.yaml"
  fi
  k -n agent-sandbox-system wait --for=condition=Available deploy --all --timeout=180s
fi

echo "=== 5. Generate ephemeral mTLS PKI (CA, connect-proxy PEP, dra-agents-net client, echo target) ==="
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "${TMPDIR}/ca.key" -out "${TMPDIR}/ca.crt" -days 1 \
  -subj "/CN=agents-net-demo-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

gen_cert() {
  local name="$1" cn="$2" san="$3" eku="$4"
  cat > "${TMPDIR}/${name}.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=${eku}
subjectAltName=${san}
EOF
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "${TMPDIR}/${name}.key" -out "${TMPDIR}/${name}.csr" \
    -subj "/CN=${cn}" 2>/dev/null
  openssl x509 -req -in "${TMPDIR}/${name}.csr" \
    -CA "${TMPDIR}/ca.crt" -CAkey "${TMPDIR}/ca.key" -CAcreateserial \
    -out "${TMPDIR}/${name}.crt" -days 1 -extfile "${TMPDIR}/${name}.ext" 2>/dev/null
}

gen_cert "pep" "connect-proxy.${NS}.svc.cluster.local" \
  "DNS:connect-proxy.${NS}.svc.cluster.local,DNS:connect-proxy.${NS}.svc,DNS:connect-proxy" \
  "serverAuth"
gen_cert "client" "dra-agents-net-demo-client" \
  "DNS:dra-agents-net-demo-client" \
  "clientAuth"
gen_cert "echo" "echo.${NS}.svc.cluster.local" \
  "DNS:echo.${NS}.svc.cluster.local,DNS:echo.${NS}.svc" \
  "serverAuth"

echo "=== 6. Install dra-agents-net in kube-system with mounted mTLS client credentials ==="
k -n kube-system delete secret demo-driver-client-tls --ignore-not-found >/dev/null 2>&1 || true
k -n kube-system delete configmap demo-ca --ignore-not-found >/dev/null 2>&1 || true
k -n kube-system create configmap demo-ca --from-file=ca.crt="${TMPDIR}/ca.crt"
k -n kube-system create secret tls demo-driver-client-tls --cert="${TMPDIR}/client.crt" --key="${TMPDIR}/client.key"

if [ -n "${AGENTS_NET_DIR}" ]; then
  sed "s|image: ghcr.io/aojea/agents.net/dra-agents-net:latest|image: ${DRIVER_IMAGE}|g" \
    "${AGENTS_NET_DIR}/kubernetes/install.yaml" | k apply -f -
else
  curl -fsSL "${INSTALL_YAML_URL}" | \
    sed "s|image: ghcr.io/aojea/agents.net/dra-agents-net:latest|image: ${DRIVER_IMAGE}|g" | \
    k apply -f -
fi

k -n kube-system patch daemonset dra-agents-net --type=strategic --patch '
spec:
  template:
    spec:
      containers:
        - name: dra-agents-net
          volumeMounts:
            - name: demo-client-tls
              mountPath: /etc/dra-agents-net/client
              readOnly: true
            - name: demo-ca
              mountPath: /etc/dra-agents-net/ca
              readOnly: true
      volumes:
        - name: demo-client-tls
          secret:
            secretName: demo-driver-client-tls
        - name: demo-ca
          configMap:
            name: demo-ca
'
k -n kube-system rollout status daemonset/dra-agents-net --timeout=120s

echo "=== 7. Deploy demo namespace, echo target, and mTLS connect-proxy PEP ==="
k apply -f "${MANIFESTS_DIR}/00-namespace.yaml"
k -n "${NS}" delete configmap demo-ca pep-policy --ignore-not-found >/dev/null 2>&1 || true
k -n "${NS}" delete secret pep-tls echo-tls --ignore-not-found >/dev/null 2>&1 || true
k -n "${NS}" create configmap demo-ca --from-file=ca.crt="${TMPDIR}/ca.crt"
k -n "${NS}" create secret tls pep-tls --cert="${TMPDIR}/pep.crt" --key="${TMPDIR}/pep.key"
k -n "${NS}" create secret tls echo-tls --cert="${TMPDIR}/echo.crt" --key="${TMPDIR}/echo.key"

cat <<EOF | k apply -f -
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: ${NS}
spec:
  selector:
    app: echo
  ports:
    - name: https
      port: 8443
      targetPort: 8443
    - name: udp-echo
      port: 8444
      targetPort: 8444
      protocol: UDP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: ${NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      containers:
        - name: echo
          image: ${PYTHON_IMAGE}
          imagePullPolicy: IfNotPresent
          command:
            - python3
            - -c
            - |
              import http.server, json, socket, ssl, threading
              def udp_echo():
                  s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                  s.bind(("0.0.0.0", 8444))
                  while True:
                      data, addr = s.recvfrom(65535)
                      s.sendto(data, addr)
              threading.Thread(target=udp_echo, daemon=True).start()
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      body = json.dumps({"ok": True, "path": self.path, "host": self.headers.get("Host")}).encode()
                      self.send_response(200)
                      self.send_header("Content-Type", "application/json")
                      self.send_header("Content-Length", str(len(body)))
                      self.end_headers()
                      self.wfile.write(body)
                  def log_message(self, *args):
                      pass
              srv = http.server.ThreadingHTTPServer(("0.0.0.0", 8443), H)
              ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
              ctx.load_cert_chain("/tls/tls.crt", "/tls/tls.key")
              srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
              srv.serve_forever()
          ports:
            - containerPort: 8443
            - containerPort: 8444
              protocol: UDP
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: echo-tls
EOF

ECHO_IP="$(k -n "${NS}" get svc echo -o jsonpath='{.spec.clusterIP}')"
if [[ "${ECHO_IP}" == *:* ]]; then
  ECHO_CIDR="${ECHO_IP}/128"
else
  ECHO_CIDR="${ECHO_IP}/32"
fi

cat > "${TMPDIR}/policy.json" <<EOF
{
  "agents_net_policy": 1,
  "version": "demo-v1",
  "sandbox": "agents-net-demo",
  "default": "deny",
  "features": {
    "h2": true,
    "udp": true
  },
  "rules": [
    {
      "id": "allow-echo-https",
      "name": "echo.${NS}.svc.cluster.local",
      "ports": [8443],
      "transports": ["tcp"]
    },
    {
      "id": "allow-echo-udp",
      "name": "echo.${NS}.svc.cluster.local",
      "ports": [8444],
      "transports": ["udp"]
    }
  ],
  "resolved_addresses": [
    {
      "cidr": "${ECHO_CIDR}",
      "ports": [8443, 8444]
    }
  ]
}
EOF
k -n "${NS}" create configmap pep-policy --from-file=policy.json="${TMPDIR}/policy.json"

cat <<EOF | k apply -f -
apiVersion: v1
kind: Service
metadata:
  name: connect-proxy
  namespace: ${NS}
spec:
  selector:
    app: connect-proxy
  ports:
    - name: https
      port: 8443
      targetPort: 8443
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: connect-proxy
  namespace: ${NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: connect-proxy
  template:
    metadata:
      labels:
        app: connect-proxy
    spec:
      containers:
        - name: connect-proxy
          image: ${PEP_IMAGE}
          imagePullPolicy: IfNotPresent
          env:
            - name: GODEBUG
              value: http2xconnect=1
          args:
            - -listen=tcp://0.0.0.0:8443
            - -h2
            - -policy=/etc/agents-net/policy.json
            - -generation=demo-gen-1
            - -tls-cert=/tls/tls.crt
            - -tls-key=/tls/tls.key
            - -tls-client-ca=/ca/ca.crt
          ports:
            - containerPort: 8443
          volumeMounts:
            - name: policy
              mountPath: /etc/agents-net
              readOnly: true
            - name: tls
              mountPath: /tls
              readOnly: true
            - name: ca
              mountPath: /ca
              readOnly: true
      volumes:
        - name: policy
          configMap:
            name: pep-policy
        - name: tls
          secret:
            secretName: pep-tls
        - name: ca
          configMap:
            name: demo-ca
EOF

k -n "${NS}" rollout status deployment/echo --timeout=90s
k -n "${NS}" rollout status deployment/connect-proxy --timeout=90s

echo "=== 8. Apply DeviceClass, ResourceClaimTemplate, and Sandbox ==="
k apply -f "${MANIFESTS_DIR}/01-device-class.yaml"
k apply -f "${MANIFESTS_DIR}/02-resource-claim-template.yaml"
k apply -f "${MANIFESTS_DIR}/03-sandbox.yaml"
k -n "${NS}" wait --for=condition=Ready sandbox/coding-agent --timeout=90s

echo "=== 9. Verify HostTun transparent TCP/UDP egress, confinement, and zero sidecar in Sandbox ==="
verify_sandbox_pod() {
  local pod="$1"
  local label="$2"
  k -n "${NS}" exec -i "${pod}" -- env ECHO_IP="${ECHO_IP}" NS="${NS}" LABEL="${label}" python3 - <<'PY'
import ipaddress, json, os, socket, urllib.error, urllib.request

label = os.environ["LABEL"]
echo_host = f"echo.{os.environ['NS']}.svc.cluster.local"

# 1. Zero proxy env vars, zero egress.sock mount, zero /dev/net/tun in container.
assert not os.environ.get("HTTP_PROXY") and not os.environ.get("HTTPS_PROXY"), "unexpected proxy env vars in HostTun"
assert not os.path.exists("/run/agents.net/egress.sock"), "unexpected egress.sock mount in HostTun"
assert not os.path.exists("/dev/net/tun"), "unexpected /dev/net/tun device in HostTun"
assert os.environ.get("SSL_CERT_FILE") == "/var/run/agent-ca.pem", f"SSL_CERT_FILE={os.environ.get('SSL_CERT_FILE')!r}"

# 2. NRI replaced /etc/resolv.conf with the HostTun virtual DNS resolver.
with open("/etc/resolv.conf") as f:
    resolv = f.read()
assert resolv == "nameserver 198.19.255.253\n", f"unexpected /etc/resolv.conf: {resolv!r}"

# 3. Non-loopback routes belong exclusively to tun0 (eth0 routes flushed).
with open("/proc/net/route") as f:
    ifaces = {ln.split()[0] for ln in f.readlines()[1:] if ln.strip()}
assert ifaces == {"tun0"}, f"expected routes only on tun0, got: {ifaces}"

# 4. VirtualDNS resolves echo into RFC 2544 synthetic range 198.18.0.0/15.
resolved_ip = socket.gethostbyname(echo_host)
assert ipaddress.ip_address(resolved_ip) in ipaddress.ip_network("198.18.0.0/15"), f"unexpected IP: {resolved_ip}"

# 5. Unmodified HTTPS client reaches allowed host through mTLS PEP.
with urllib.request.urlopen(f"https://{echo_host}:8443/{label}", timeout=10) as resp:
    data = json.loads(resp.read().decode())
    assert resp.status == 200 and data["ok"] is True and data["path"] == f"/{label}", data
    print(f"PASS [{label}] transparent HTTPS via HostTun + mTLS PEP:", data)

# 6. Unmodified UDP socket echoes through RFC 9298 connect-udp.
payload = f"udp-{label}".encode()
u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
u.settimeout(10)
u.sendto(payload, (echo_host, 8444))
echoed, _ = u.recvfrom(4096)
u.close()
assert echoed == payload, f"UDP echo mismatch: {echoed!r}"
print(f"PASS [{label}] transparent UDP via HostTun + connect-udp:", echoed.decode())

# 7. Denied host fails closed at the PEP.
try:
    urllib.request.urlopen("https://blocked.example:443/", timeout=5)
    raise SystemExit("FAIL: blocked.example unexpectedly succeeded")
except (urllib.error.URLError, OSError) as e:
    print(f"PASS [{label}] denied blocked.example:", e)

# 8. Unmapped synthetic IP fails closed locally before touching PEP.
try:
    socket.create_connection(("198.18.9.9", 443), timeout=5).close()
    raise SystemExit("FAIL: unmapped synthetic IP 198.18.9.9 unexpectedly succeeded")
except OSError as e:
    print(f"PASS [{label}] unmapped synthetic IP 198.18.9.9 failed closed:", e)
PY
}

verify_sandbox_pod "coding-agent" "sandbox-initial"

echo "=== 10. Verify Sandbox suspend/resume reclaims and recreates ResourceClaim + HostTun ==="
k -n "${NS}" patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Suspended"}}'
k -n "${NS}" wait --for=delete pod/coding-agent --timeout=60s
for _ in $(seq 1 20); do
  COUNT="$(k -n "${NS}" get resourceclaims -o name | wc -l | tr -d ' ')"
  if [ "${COUNT}" = "0" ]; then
    break
  fi
  sleep 0.5
done
COUNT="$(k -n "${NS}" get resourceclaims -o name | wc -l | tr -d ' ')"
if [ "${COUNT}" != "0" ]; then
  echo "FAIL: expected 0 ResourceClaims while Sandbox suspended, got ${COUNT}" >&2
  exit 1
fi
echo "PASS Sandbox suspend released Pod-owned ResourceClaim"

k -n "${NS}" patch sandbox coding-agent --type=merge -p '{"spec":{"operatingMode":"Running"}}'
k -n "${NS}" wait --for=condition=Ready sandbox/coding-agent --timeout=90s
verify_sandbox_pod "coding-agent" "sandbox-resumed"

echo "=== 11. Verify SandboxTemplate + SandboxWarmPool + SandboxClaim adoption ==="
k apply -f "${MANIFESTS_DIR}/04-warmpool.yaml"
k -n "${NS}" wait --for=condition=Ready sandboxclaim/coding-session --timeout=120s
CLAIMED_SANDBOX="$(k -n "${NS}" get sandboxclaim coding-session -o jsonpath='{.status.sandbox.name}{.status.sandbox.Name}')"
echo "SandboxClaim coding-session adopted Sandbox=${CLAIMED_SANDBOX}"
verify_sandbox_pod "${CLAIMED_SANDBOX}" "warmpool-claim"

echo "=== 12. Verify connect-proxy JSONL audit log (mTLS peer, h2 TCP/UDP, deny, zero synthetic leaks) ==="
PEP_LOGS="$(k -n "${NS}" logs deploy/connect-proxy)"
python3 - "${PEP_LOGS}" <<'PY'
import json, sys
records = [json.loads(ln) for ln in sys.argv[1].splitlines() if ln.strip().startswith("{")]
tcp_allows = [r for r in records if r.get("decision") == "allow" and r.get("transport") == "tcp" and r.get("peer") == "dra-agents-net-demo-client" and r.get("wire") == "h2"]
udp_allows = [r for r in records if r.get("decision") == "allow" and r.get("transport") == "udp" and r.get("peer") == "dra-agents-net-demo-client" and r.get("wire") == "h2"]
blocks = [r for r in records if r.get("decision") == "block" and r.get("destination") == "blocked.example:443" and r.get("reason") == "not-on-allowlist"]
leaks = [r for r in records if "198.18.9.9" in str(r.get("destination", ""))]
assert len(tcp_allows) >= 3, f"expected >= 3 TCP allow records (initial, resumed, warmpool), got {len(tcp_allows)}"
assert len(udp_allows) >= 3, f"expected >= 3 UDP allow records, got {len(udp_allows)}"
assert blocks, f"missing block record for blocked.example:443: {records}"
assert not leaks, f"unmapped synthetic IP leaked to PEP: {leaks}"
print(f"PASS connect-proxy audit: {len(tcp_allows)} TCP allow(s), {len(udp_allows)} UDP allow(s), {len(blocks)} block(s), 0 synthetic leaks")
PY

echo "=== ALL AGENT-SANDBOX + DRA.AGENTS.NET DEMO CHECKS PASSED ==="
