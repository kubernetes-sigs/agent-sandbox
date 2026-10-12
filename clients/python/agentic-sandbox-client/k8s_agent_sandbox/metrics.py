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

_LATENCY_BUCKETS_SECONDS = (0.1, 0.25, 0.5, 0.75, 1, 1.25, 1.5, 2, 2.5, 5, 10, 30, 60, 120, 240)

sandbox_client_discovery_latency_seconds = Histogram(
    'sandbox_client_discovery_latency_seconds',
    'Latency of establishing connection to the sandbox in seconds',
    ['mode', 'status'],
    buckets=_LATENCY_BUCKETS_SECONDS
)

sandbox_client_suspend_latency_seconds = Histogram(
    'sandbox_client_suspend_latency_seconds',
    'Latency of suspending the sandbox in seconds',
    ['status'],
    buckets=_LATENCY_BUCKETS_SECONDS
)

sandbox_client_resume_latency_seconds = Histogram(
    'sandbox_client_resume_latency_seconds',
    'Latency of resuming the sandbox in seconds',
    ['status'],
    buckets=_LATENCY_BUCKETS_SECONDS
)

sandbox_client_restore_latency_seconds = Histogram(
    'sandbox_client_restore_latency_seconds',
    'Latency of restoring the sandbox from a snapshot in seconds',
    ['status'],
    buckets=_LATENCY_BUCKETS_SECONDS
)


