# Release Process

`agent-sandbox` uses repository-level semantic version tags (for example, `v0.1.0`).
Those tags are the source of truth for:

- Controller manifests published on GitHub Releases
- The Go SDK at `sigs.k8s.io/agent-sandbox/clients/go/sandbox`
- The Python SDK workflows that are triggered by `v*` tags

## Repository Release Flow

The project is released on an as-needed basis. The current process is:

1. Run `make release-promote TAG=vX.Y.Z` to create the repository tag, wait for the tagged image to be pushed, and generate the image promotion PR. Creating the Git tag also triggers the Python SDK release workflow.
1. Wait for the image promotion PR to be approved and merged.
1. Run `make release-publish TAG=vX.Y.Z` to generate the release manifests and publish the GitHub Release as a draft.
1. Review and edit the draft GitHub Release, then publish it.
1. Approve the Python publishing workflow manually.

These steps are being automated in GitHub Actions so that a release only requires adding a repository tag.

## Go SDK Releases

The Go SDK currently lives inside the repository's root Go module, so it does not have an independent module tag.
To release the Go SDK, push the same repository tag that you want users to install:

```bash
make release-go-sdk TAG=vX.Y.Z
```

After the tag is pushed:

1. The `Release Go Client` workflow runs `go test ./clients/go/sandbox/...`.
1. The workflow builds the example programs under `clients/go/examples/`.
1. The workflow refreshes the draft GitHub Release for that tag.

Consumers can then install the SDK with:

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox@vX.Y.Z
```

or track the latest repository release with:

```bash
go get sigs.k8s.io/agent-sandbox/clients/go/sandbox@latest
```
