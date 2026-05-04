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

from prometheus_client import Histogram

sandbox_client_discovery_latency_ms = Histogram(
    'sandbox_client_discovery_latency_ms',
    'Latency of establishing connection to the sandbox in milliseconds',
    ['mode', 'status'],
    buckets=(100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000)
)
