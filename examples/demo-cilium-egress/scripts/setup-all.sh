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

# One-shot: provision everything (cluster -> Cilium -> agent-sandbox -> demo).
# Idempotent: safe to re-run; existing pieces are skipped.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bash "$here/01-create-cluster.sh"
bash "$here/02-install-cilium.sh"
bash "$here/03-install-agent-sandbox.sh"
bash "$here/04-deploy-demo.sh"
echo
echo "Setup complete. Run $here/test.sh to execute the e2e verification."
