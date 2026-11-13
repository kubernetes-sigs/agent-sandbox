# Copyright 2026 The Kubernetes Authors.
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

"""Real-time filesystem event streaming for sandboxd sandboxes.

This package provides both synchronous (:class:`FileWatcher`) and
asynchronous (:class:`AsyncFileWatcher`) interfaces to the sandboxd
``watcher.v1.FileWatcherService`` gRPC API. Both classes subscribe to a
directory and yield :class:`~k8s_agent_sandbox.models.FileEvent` instances
for every observed filesystem change (create, write, remove, rename, chmod).

Requires the ``grpc`` extra::

    pip install k8s-agent-sandbox[grpc]
"""

# This package intentionally has no top-level imports. The async module
# pulls in httpx (via async_connector), which is only installed with the
# ``async`` extra. Eagerly importing AsyncFileWatcher here would break
# base installs that only need the synchronous FileWatcher — sandbox.py
# and async_sandbox.py import the submodules directly to stay lazy.

__all__ = []
