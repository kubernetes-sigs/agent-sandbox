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

"""Check that contributors can find the example policy from the project README."""

import re
from pathlib import Path


def test_examples_index_documents_contribution_policy():
    repo = Path(__file__).resolve().parents[2]
    readme = (repo / "README.md").read_text(encoding="utf-8")
    examples_link = re.search(r"\[Examples\]\(([^)]+)\)", readme)
    assert examples_link, "The project README must link to the examples index"
    index = (repo / examples_link[1] / "README.md").read_text(encoding="utf-8")

    _, heading, remainder = index.partition("\n## Example policy\n")
    assert heading, "The examples index is missing its Example policy section"
    policy, _, _ = remainder.partition("\n## ")
    topics = set(re.findall(r"^- \*\*([^*]+)\.\*\* \S", policy, re.MULTILINE))
    assert {"Admission", "Ownership", "Verification", "Removal", "Size", "Images"} <= topics
