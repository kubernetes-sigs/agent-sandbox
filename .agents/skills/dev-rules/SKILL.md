---
name: dev-rules
description: Enforces project-specific development rules and conventions.
---

# Development Rules

## Purpose
This skill ensures that AI agents contributing to this project follow project-specific rules regarding code review and style to maintain compliance and quality.

## Instructions
1.  **Go Development**: Follow standard Kubernetes Go patterns and best practices.
2.  **AI-Assisted Code Reviews**:
    *   Do **NOT** directly apply code suggestions from Copilot via the GitHub UI. Doing so adds Copilot as a co-author to the commit. Since Copilot cannot sign the Kubernetes CLA, this will cause the CLA check to fail and block the PR. Manual review and application of suggestions are required.
    *   Once the initial AI review (e.g., by Copilot) is complete, ensure all review comments are addressed and resolved so the PR can be labeled `ready-for-review` for maintainers.
3.  **API Conventions**: Always use the `k8s-api-conventions` skill when modifying APIs.
4.  **Creating Pull Requests**: When creating a Pull Request, always use and fill out the project's PR template ([`.github/pull_request_template.md`](../../../.github/pull_request_template.md)).
    *   **Description**: Provide a clear and concise description in the "What this PR does" section.
    *   **Related Issues**: Link related issues using "Fixes #issue" or "Ref link".
    *   **Release Notes**: Fill out the `release-note` block. Note that automated release notes are generated from the description in the first section, so ensure it is comprehensive. For breaking changes, describe required actions and ask the maintainers to label the PR with `release-note-action-required`.

## References
- Project rules (as documented in [`CONTRIBUTING.md`](../../../CONTRIBUTING.md))
