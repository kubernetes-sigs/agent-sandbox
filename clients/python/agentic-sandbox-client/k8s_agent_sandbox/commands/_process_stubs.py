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

"""Import shim for the generated ProcessService gRPC stubs.

The gRPC Python plugin emits absolute imports rooted at the proto package
path (``from process.v1 import process_pb2``). The stubs are vendored under
``k8s_agent_sandbox/_proto`` (see that dir's README for generation), so we
add ``_proto`` to ``sys.path`` and import via the proto package path, then
re-export. This avoids rewriting generated code and keeps the one messy
detail in a single place.

Importing this module raises ImportError when the stubs are not generated or
grpcio/protobuf are not installed; callers wrap that with a friendly message.
"""

import os
import sys

_PROTO_ROOT = os.path.join(os.path.dirname(os.path.dirname(__file__)), "_proto")
if _PROTO_ROOT not in sys.path:
    sys.path.insert(0, _PROTO_ROOT)

from process.v1 import process_pb2, process_pb2_grpc  # noqa: E402

__all__ = ["process_pb2", "process_pb2_grpc"]
