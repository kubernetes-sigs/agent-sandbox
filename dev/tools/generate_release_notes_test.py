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

"""Unit tests for the contributor list in dev/tools/generate-release-notes."""

import importlib.util
import os
import unittest
from importlib.machinery import SourceFileLoader

# generate-release-notes is an extensionless script, so load it via importlib
# with an explicit SourceFileLoader. It is import-safe: main() only runs under
# `if __name__ == "__main__"`, and google-genai is imported lazily.
_SCRIPT_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "generate-release-notes"
)
_loader = SourceFileLoader("generate_release_notes", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("generate_release_notes", _loader)
generate_release_notes = importlib.util.module_from_spec(_spec)
_loader.exec_module(generate_release_notes)


class GetContributorsTest(unittest.TestCase):
    def test_skips_bot_authors(self):
        # Author objects as `gh pr view --json author` returns them.
        prs_data = {
            2013: {"author": {"is_bot": True, "login": "app/dependabot"}},
            2079: {
                "author": {
                    "id": "MDQ6VXNlcjQ4NzY4Njc=",
                    "is_bot": False,
                    "login": "janetkuo",
                    "name": "Janet Kuo",
                }
            },
        }
        self.assertEqual(
            generate_release_notes.get_contributors(prs_data), ["@janetkuo"]
        )

    def test_dedupes_and_sorts_handles(self):
        prs_data = {
            1: {"author": {"is_bot": False, "login": "tomergee"}},
            2: {"author": {"is_bot": False, "login": "ericcurtin"}},
            3: {"author": {"is_bot": False, "login": "tomergee"}},
        }
        self.assertEqual(
            generate_release_notes.get_contributors(prs_data),
            ["@ericcurtin", "@tomergee"],
        )


if __name__ == "__main__":
    unittest.main()
