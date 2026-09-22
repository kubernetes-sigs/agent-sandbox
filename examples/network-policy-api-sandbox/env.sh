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

# Shared configuration for the ClusterNetworkPolicy + agent-sandbox codelab.
# Override any version or the cluster name by exporting it before sourcing, e.g.
#   KIND_CLUSTER_NAME=cnp-demo ./scripts/setup-all.sh

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agent-sandbox-cnp}"
# Empty => the default node image of your kind release.
export KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-}"

# network-policy-api release providing the ClusterNetworkPolicy CRD. The
# experimental channel is required for `domainNames`.
export NETWORK_POLICY_API_VERSION="${NETWORK_POLICY_API_VERSION:-v0.2.0}"

# kube-network-policies release: both the install manifest and the image tag.
export KUBE_NETWORK_POLICIES_VERSION="${KUBE_NETWORK_POLICIES_VERSION:-v1.1.1}"

# agent-sandbox release tag the walkthrough was verified with. Set to "latest" to
# auto-discover the newest GitHub release instead.
export AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v1.0.2}"

# Not overridable: the namespaces are hard-coded in manifests/*.yaml and in the
# README, so an override here would only desynchronise the scripts from them.
export NS_A=sandbox-team-a
export NS_B=sandbox-team-b
export NS_TOOLS=shared-tools

# The ClusterNetworkPolicy manifests this example owns, in removal order. The
# scripts delete exactly these and never touch other policies in the cluster.
# shellcheck disable=SC2034 # consumed by the scripts that source this file
CNP_MANIFESTS=(80-cnp-baseline-default-deny 70-cnp-admin-pass-shared-tools 60-cnp-admin-team-b-allow-pypi
               50-cnp-admin-allow-github 40-cnp-admin-allow-dns 30-cnp-admin-default-deny)
