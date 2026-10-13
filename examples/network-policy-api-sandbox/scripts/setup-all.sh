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

# Provision everything: kind cluster -> network policy API + enforcer ->
# agent-sandbox -> demo workloads. Idempotent.
set -euo pipefail
D="$(dirname "${BASH_SOURCE[0]}")"
"$D/01-create-cluster.sh"
"$D/02-install-network-policies.sh"
"$D/03-install-agent-sandbox.sh"
"$D/04-deploy-demo.sh"
echo
echo "Setup complete. Follow the README, or run ./scripts/test.sh to check every phase."
