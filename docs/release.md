# Release Guide

This guide covers the release process for `agent-sandbox`, including image promotion and the automated release workflow.

## Image Registries and Promotion

The project uses Google Artifact Registry (GAR) for container image storage and distribution.

### Registries

-   **Staging Registry**: `us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox`.
    This is where all intermediate and development images are pushed as postsubmits. See [post-agent-sandbox-push-images job history](https://prow.k8s.io/job-history/gs/kubernetes-ci-logs/logs/post-agent-sandbox-push-images).
-   **Production Registry**: `registry.k8s.io/agent-sandbox`.
    Official releases are served from this registry.

### Promotion Process

To move an image from staging to the production registry, a **promotion process** is required:

1.  **Staging**: Images are built and pushed to the staging registry.
2.  **Promotion PR**: A PR is submitted to the [kubernetes/k8s.io](https://github.com/kubernetes/k8s.io) repository. This PR updates the registry configuration (e.g., [`registry.k8s.io/images/k8s-staging-agent-sandbox/images.yaml`](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/images/k8s-staging-agent-sandbox/images.yaml)) with the image digest and its associated tag. See [example PR](https://github.com/kubernetes/k8s.io/pull/9230).
3.  **Promotion**: Once the PR is merged, the image is automatically promoted to `registry.k8s.io`.

This step can be automated by running `make release-promote TAG=vX.Y.Z`. This calls [`dev/tools/tag-promote-images`](../dev/tools/tag-promote-images) script which handles the promotion process.

> [!IMPORTANT]
> `make release-promote` by default also creates and pushes the git tag. You can use `SKIP_TAGGING=true` to skip tagging, or `ONLY_TAGGING=true` to only perform tagging. It requires `gh` and `gcloud` authentication.

## Automated Release Workflow

The project uses a GitHub Actions workflow to automate the release process: [`.github/workflows/release.yml`](../.github/workflows/release.yml).

### Overview

The workflow performs the following steps:

1.  **Tag Release**: Tags the release. If a tag is not provided in the workflow input, it uses [`./dev/tools/auto-tag`](../dev/tools/auto-tag) to determine the next patch version based on commits since the last tag.
2.  **Promote Images**: Creates a PR to [kubernetes/k8s.io](https://github.com/kubernetes/k8s.io) to promote the image from staging to production.
3.  **Poll PR Status**: Waits for the promotion PR to be merged. This step requires human reviewers to approve the PR.
4.  **Publish Draft Release**: Once the PR is merged, it generates a draft release on GitHub with generated manifests by running `make release-publish`.

### Triggering the Workflow

The workflow is triggered manually via `workflow_dispatch` (requires write access to the repository).

> [!NOTE]
> While the workflow is currently triggered manually, the plan is to automate this to run periodically for patch releases (e.g., every Friday).

**Inputs:**

-   `tag`: The tag for the release (e.g., `v0.2.0`). Leave blank for auto-patch.
-   `poll_timeout_mins`: Maximum minutes to poll for PR merge (default: `360`).

**To trigger the workflow:**

1.  Go to the **Actions** tab in the GitHub repository.
2.  Select **Scheduled Release Automation**.
3.  Click **Run workflow**.
4.  Fill in the optional tag or leave blank for auto-patch.

## Release Notes Generation

The release process automatically generates release notes using the Gemini API. The script [`dev/tools/generate-release-notes`](../dev/tools/generate-release-notes) gathers information and sends it to Gemini to create a summary.

### Information Used by Gemini

Gemini uses the following information to generate release notes:

-   **Commit Titles and Messages**: All commits between the previous release and the current release.
-   **PR Descriptions**: The body/description of all merged Pull Requests associated with those commits (fetched via `gh pr view`).
-   **Breaking Changes**: Specifically, the body of PRs labeled with `release-note-action-required`.

### Guidance for PR Authors

To ensure high-quality automated release notes, PR authors should follow these best practices:

-   **Write Clear PR Titles and Descriptions**: Since Gemini synthesizes PR descriptions, providing a clear and comprehensive summary of the changes in the PR body helps generate accurate highlights.
-   **Document Breaking Changes Clearly**: If a PR introduces breaking changes or requires user action, add the `release-note-action-required` label and clearly describe the impact and necessary actions in the PR body.
