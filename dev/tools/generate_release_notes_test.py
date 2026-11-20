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

"""Unit tests for dev/tools/generate-release-notes."""

import importlib.util
import os
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

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


class SelectPreviousReleaseTagTest(unittest.TestCase):
    def test_prefers_latest_when_different_from_current(self):
        releases = [
            {"tagName": "v1.0.12", "isLatest": False, "isDraft": True, "isPrerelease": False},
            {"tagName": "v1.0.11", "isLatest": True, "isDraft": False, "isPrerelease": False},
            {"tagName": "v1.0.10", "isLatest": False, "isDraft": False, "isPrerelease": False},
        ]
        self.assertEqual(
            generate_release_notes.select_previous_release_tag(releases, "v1.0.12"),
            "v1.0.11",
        )

    def test_skips_current_tag_when_already_latest(self):
        releases = [
            {"tagName": "v1.0.12", "isLatest": True, "isDraft": False, "isPrerelease": False},
            {"tagName": "v1.0.12-rc1", "isLatest": False, "isDraft": False, "isPrerelease": True},
            {"tagName": "v1.0.11", "isLatest": False, "isDraft": False, "isPrerelease": False},
        ]
        self.assertEqual(
            generate_release_notes.select_previous_release_tag(releases, "v1.0.12"),
            "v1.0.11",
        )


class CommitClassificationAndSignalsTest(unittest.TestCase):
    def test_classify_commit_files(self):
        self.assertEqual(
            generate_release_notes.classify_commit_files(
                ["docs/api.md", "site/content/docs/troubleshooting/_index.md"]
            ),
            "docs-only",
        )
        self.assertEqual(
            generate_release_notes.classify_commit_files(
                ["clients/go/sandbox/client_test.go", "clients/go/sandbox/sandbox_test.go"]
            ),
            "tests-only",
        )
        self.assertEqual(
            generate_release_notes.classify_commit_files(
                ["dev/ci/shared/kops-gcp-lifecycle.sh"]
            ),
            "ci-or-tooling-only",
        )
        self.assertEqual(
            generate_release_notes.classify_commit_files(
                ["site/package.json", "site/package-lock.json"], is_bot=True
            ),
            "bot-dependency-update",
        )
        self.assertEqual(
            generate_release_notes.classify_commit_files(
                ["packages/sandboxd/pkg/server/filesystem.go", "packages/sandboxd/pkg/server/filesystem_test.go"]
            ),
            "code-or-manifests",
        )

    def test_parse_helm_chart_version_change(self):
        diff = (
            "--- a/helm/Chart.yaml\n"
            "+++ b/helm/Chart.yaml\n"
            "@@ -1,5 +1,5 @@\n"
            " apiVersion: v2\n"
            " name: agent-sandbox\n"
            "-version: 0.1.4\n"
            "+version: 0.1.5\n"
        )
        self.assertEqual(
            generate_release_notes.parse_helm_chart_version_change(diff),
            ("0.1.4", "0.1.5"),
        )
        self.assertIsNone(
            generate_release_notes.parse_helm_chart_version_change("no version change")
        )

    def test_truncate_text_on_newline(self):
        text = "line1\nline2\nline3\nline4"
        truncated = generate_release_notes.truncate_text(text, 12)
        self.assertTrue(truncated.startswith("line1\nline2\n... [diff truncated,"))

    def test_get_pr_numbers_preserves_order(self):
        commits = [
            {"title": "feat: second (#20)", "pr_number": 20},
            {"title": "feat: first (#10)", "pr_number": 10},
            {"title": "fix: dup (#20)", "pr_number": 20},
        ]
        self.assertEqual(generate_release_notes.get_pr_numbers(commits), [20, 10])


class FooterAndAssemblyTest(unittest.TestCase):
    def test_extract_github_footer_drops_whats_changed(self):
        generated = (
            "## What's Changed\n"
            "* feat: something by @alice in https://github.com/kubernetes-sigs/agent-sandbox/pull/1\n\n"
            "## New Contributors\n"
            "* @alice made their first contribution in https://github.com/kubernetes-sigs/agent-sandbox/pull/1\n\n"
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12\n"
        )
        footer = generate_release_notes.extract_github_footer(
            generated, "v1.0.11", "v1.0.12"
        )
        self.assertNotIn("What's Changed", footer)
        self.assertIn("### New Contributors\n* @alice", footer)
        self.assertIn(
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12",
            footer,
        )

    def test_extract_github_footer_without_new_contributors(self):
        generated = (
            "## What's Changed\n"
            "* feat: something by @alice in https://github.com/kubernetes-sigs/agent-sandbox/pull/1\n\n"
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12\n"
        )
        footer = generate_release_notes.extract_github_footer(
            generated, "v1.0.11", "v1.0.12"
        )
        self.assertEqual(
            footer,
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12",
        )

    def test_extract_github_footer_fallback_when_empty(self):
        footer = generate_release_notes.extract_github_footer("", "v1.0.11", "v1.0.12")
        self.assertEqual(
            footer,
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12",
        )

    def test_strip_markdown_fences(self):
        wrapped = "```markdown\nHello world\n```"
        self.assertEqual(
            generate_release_notes.strip_markdown_fences(wrapped), "Hello world"
        )
        self.assertEqual(
            generate_release_notes.strip_markdown_fences("Plain text"), "Plain text"
        )

    def test_assemble_release_notes_strips_duplicate_sections_and_appends_footer(self):
        gemini_output = (
            "Summary paragraph.\n\n"
            "### Installation\ninstall cmds\n\n"
            "### Contributors\n@alice\n\n"
            "## What's Changed\n* junk\n\n"
            "### New Contributors\n* @hallucinated\n\n"
            "**Full Changelog**: https://example.com/wrong"
        )
        footer = (
            "### New Contributors\n"
            "* @alice made their first contribution in https://github.com/kubernetes-sigs/agent-sandbox/pull/1\n\n"
            "**Full Changelog**: https://github.com/kubernetes-sigs/agent-sandbox/compare/v1.0.11...v1.0.12"
        )
        result = generate_release_notes.assemble_release_notes(
            tag="v1.0.12",
            gemini_notes=gemini_output,
            installation_notes="### Installation\ninstall cmds",
            contributors=["@alice"],
            footer=footer,
        )
        self.assertNotIn("What's Changed", result)
        self.assertNotIn("@hallucinated", result)
        self.assertNotIn("example.com/wrong", result)
        self.assertIn("### New Contributors\n* @alice", result)
        self.assertEqual(result.count("**Full Changelog**:"), 1)

    def test_enrich_commits_skips_diff_for_tests_and_bots(self):
        commits = [
            {"sha": "aaa11111", "title": "cleanup: tests (#1)", "body": "", "pr_number": 1},
            {"sha": "bbb22222", "title": "build(deps): bump (#2)", "body": "", "pr_number": 2},
            {"sha": "ccc33333", "title": "feat: real change (#3)", "body": "", "pr_number": 3},
        ]
        prs_data = {
            1: {"author": {"is_bot": False, "login": "alice"}, "labels": [], "body": "test cleanup"},
            2: {"author": {"is_bot": True, "login": "app/dependabot"}, "labels": [], "body": "bump"},
            3: {"author": {"is_bot": False, "login": "bob"}, "labels": [], "body": "real pr"},
        }
        files_by_sha = {
            "aaa11111": ["clients/go/sandbox/client_test.go"],
            "bbb22222": ["site/package.json"],
            "ccc33333": ["packages/sandboxd/pkg/server/filesystem.go"],
        }
        with mock.patch.object(
            generate_release_notes,
            "get_commit_files",
            side_effect=lambda sha: files_by_sha[sha],
        ), mock.patch.object(
            generate_release_notes,
            "get_commit_stat",
            return_value="1 file changed",
        ), mock.patch.object(
            generate_release_notes,
            "get_commit_diff",
            return_value="+added line",
        ) as mock_diff:
            enriched = generate_release_notes.enrich_commits(commits, prs_data)

        self.assertEqual(enriched[0]["scope"], "tests-only")
        self.assertEqual(enriched[0]["diff"], "")
        self.assertEqual(enriched[1]["scope"], "bot-dependency-update")
        self.assertEqual(enriched[1]["diff"], "")
        self.assertEqual(enriched[2]["scope"], "code-or-manifests")
        self.assertEqual(enriched[2]["diff"], "+added line")
        mock_diff.assert_called_once_with(
            "ccc33333", max_chars=generate_release_notes.MAX_DIFF_CHARS_PER_COMMIT
        )


if __name__ == "__main__":
    unittest.main()
