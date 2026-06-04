# Release Process

`agent-sandbox` uses repository-level semantic version tags (for example, `v0.1.0`).
Those tags are the source of truth for:

- Controller manifests published on GitHub Releases
- The Go SDK at `sigs.k8s.io/agent-sandbox/clients/go/sandbox`
- The Python SDK workflows that are triggered by `v*` tags

## Repository Release Flow

The project is released on an as-needed basis. The current process is:

1. Run `make release-promote TAG=vX.Y.Z` to create the repository tag, wait for the tagged image to be pushed, and generate the image promotion PR. Creating the Git tag also triggers the Python SDK workflows.
1. Wait for the image promotion PR to be approved and merged.
1. Run `make release-publish TAG=vX.Y.Z` to generate the release manifests and publish the GitHub Release as a draft.
1. Review and edit the draft GitHub Release, then publish it.
1. Approve the Python publishing workflow manually.

These steps are being automated in [GitHub Actions](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/.github/workflows/release.yml) so that a release only requires adding a repository tag.
