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
# Thin wrapper around the canonical migration script bundled with the Helm
# chart. The real implementation lives at helm/files/migrate.sh so the chart's
# ConfigMap template can include it via .Files.Get without going outside the
# chart directory. Operators are expected to invoke this wrapper from the
# repo root:
#
#   bash dev/tools/migrate.sh --phase=bootstrap [--dry-run]
#
# See docs/api-migration-guide.md for full usage.
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${SELF_DIR}/../../helm/files/migrate.sh" "$@"
