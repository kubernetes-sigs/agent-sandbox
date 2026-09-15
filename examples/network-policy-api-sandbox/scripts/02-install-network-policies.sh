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

# Install the ClusterNetworkPolicy CRD (network-policy-api) and the
# kube-network-policies DaemonSet that enforces it.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../env.sh"

npa_raw="https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/refs/tags/${NETWORK_POLICY_API_VERSION}"
knp_raw="https://raw.githubusercontent.com/kubernetes-sigs/kube-network-policies/${KUBE_NETWORK_POLICIES_VERSION}"

echo "Installing ClusterNetworkPolicy CRD (network-policy-api ${NETWORK_POLICY_API_VERSION}, experimental channel) ..."
# The experimental channel is the one with the domainNames egress peer.
kubectl apply -f "${npa_raw}/config/crd/experimental/policy.networking.k8s.io_clusternetworkpolicies.yaml"
kubectl wait --for=condition=Established crd/clusternetworkpolicies.policy.networking.k8s.io --timeout=60s

echo "Installing kube-network-policies ${KUBE_NETWORK_POLICIES_VERSION} (ClusterNetworkPolicy variant) ..."
# install-cnp.yaml does not include the CRDs. The manifest is taken from the
# release tag and the image pinned to the same release.
# The upstream default is --fail-open=true (the nfqueue rule carries the bypass
# flag), which lets traffic through while the agent is down or restarting. This
# example is about a default-deny guardrail, so make it fail closed; the
# trade-off is that pod traffic is dropped if the DaemonSet pod crashes. Node
# traffic from root (kubelet, containerd) is not queued and is unaffected.
image="registry.k8s.io/networking/kube-network-policies:${KUBE_NETWORK_POLICIES_VERSION}-npa-v1alpha2"
manifest="$(curl -fsSL "${knp_raw}/install-cnp.yaml" \
  | sed -E "s#image: registry.k8s.io/networking/kube-network-policies:.*#image: ${image}#" \
  | awk '{ print } /^ *- \/bin\/netpol$/ { sub(/\/bin\/netpol/, "--fail-open=false"); print }')"
# Both rewrites match on the upstream layout and would no-op silently if it
# changes; the default deny would then quietly be fail-open again.
for want in "image: ${image}" "- --fail-open=false"; do
  grep -qF -- "$want" <<<"$manifest" \
    || { echo "ERROR: rendered install-cnp.yaml does not contain '${want}'; the upstream manifest layout changed" >&2; exit 1; }
done
kubectl apply -f - <<<"$manifest"
kubectl rollout status daemonset/kube-network-policies -n kube-system --timeout=180s
echo "OK: ClusterNetworkPolicy API and kube-network-policies installed."
