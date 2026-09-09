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

from .termination_fn import TerminationFn


class SparseTaskTermination(TerminationFn):
    """
    Invokes success_fn(obs, info) internally that returns True if terminated and False otherwise.

    Example:
        termination_fn = SparseTaskTermination(success_fn=lambda obs, info: "done" in obs)
    """
    def __init__(self, success_fn):
        self.success_fn = success_fn

    def __call__(self, obs, info, task) -> bool:
        return bool(self.success_fn(obs, info))
