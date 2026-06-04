# Contributing Guidelines

Welcome to Kubernetes. We are excited about the prospect of you joining our [community](https://git.k8s.io/community)! The Kubernetes community abides by the CNCF [code of conduct](code-of-conduct.md). Here is an excerpt:

_As contributors and maintainers of this project, and in the interest of fostering an open and welcoming community, we pledge to respect all people who contribute through reporting issues, posting feature requests, updating documentation, submitting pull requests or patches, and other activities._

## Getting Started

We have full documentation on how to get started contributing here:

- [Contributor License Agreement](https://git.k8s.io/community/CLA.md) - Kubernetes projects require that you sign a Contributor License Agreement (CLA) before we can accept your pull requests
- [Kubernetes Contributor Guide](https://k8s.dev/guide) - Main contributor documentation, or you can just jump directly to the [contributing page](https://k8s.dev/docs/guide/contributing/)
- [Contributor Cheat Sheet](https://k8s.dev/cheatsheet) - Common resources for existing developers

## Pull Request and Code Review Policy

To maintain high velocity and prevent our review queue from stagnating, this project enforces the following guidelines for all Pull Requests:

- **CLA Requirement:** All contributors must sign the [Contributor License Agreement](https://git.k8s.io/community/CLA.md). PRs without a signed CLA will not be reviewed.
- **AI-Assisted Code Reviews:** We use GitHub Copilot and CodeRabbit to provide automated, first-pass code reviews to help identify low-hanging fruit and improve review velocity.
  - **⚠️ Important Contribution Note (CLA Requirement):** If Copilot or CodeRabbit provides a code suggestion in your PR, **do not click the "Commit suggestion" button** in the GitHub UI. Doing so adds the AI bot as a co-author to the commit. Since bots cannot sign the Kubernetes CLA, this will cause the CLA check to fail and block your PR. Instead, please manually apply the suggested changes in your local environment and push the commit yourself.
  - **GitHub Copilot:** If your organization or GitHub account has Copilot enabled, you can interact with Copilot directly in the PR comment threads by tagging `@copilot` or clicking the Copilot sparkle icon to ask questions, explain code, or suggest fixes.
  - **CodeRabbit:** CodeRabbit provides automated PR summaries, walkthroughs, and high-level reviews. It is configured to signal approval once checks pass and issues are resolved. You can interact with it by commenting `@coderabbitai` on your PR (e.g., `@coderabbitai review` to request an incremental re-review, or `@coderabbitai full review` to re-evaluate the entire PR from scratch).
  - **AI Agent Skills:** We provide instructions for AI agents in the [`.agents/skills/`](.agents/skills/) directory. These skills guide AI assistants to follow project conventions and rules. Contributors using AI tools are encouraged to reference these skills.
- **Fast-Track Delivery (PR Takeovers):** To focus on delivering features faster, maintainers may take over community PRs that are approved or highly important. If a PR is close to completion, a maintainer might push the final changes and merge it directly.
- **Stale Management:** We have shifted to a more aggressive rule for inactive PRs to reduce queue clutter. Any PR that is inactive for 30 days will be automatically marked stale (`lifecycle/stale` label) and closed after 15 more days of inactivity. Closed PRs can always be reopened if the author returns to continue the work.

## Mentorship

- [Mentoring Initiatives](https://k8s.dev/community/mentoring) - We have a diverse set of mentorship programs available that are always looking for volunteers!

## Contact Information

- [Slack channel](https://kubernetes.slack.com/messages/sig-apps)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-apps)

