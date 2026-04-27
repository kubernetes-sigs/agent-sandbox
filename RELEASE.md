# Release Process

`agent-sandbox` uses repository-level semantic version tags (for example, `v0.1.0`).
Those tags are the source of truth for:

- Controller manifests published on GitHub Releases
- The Go SDK at `sigs.k8s.io/agent-sandbox/clients/go/sandbox`
- The Python SDK workflows that are triggered by `v*` tags

## Repository Release Flow

The project is released on an as-needed basis. The process is as follows:

1. An issue proposes a new release with a changelog since the last release.
1. All [OWNERS](OWNERS) must LGTM the release.
1. An OWNER promotes images and prepares release assets as needed.
1. An OWNER pushes the repository tag for the release.
1. GitHub Actions validates the Go SDK and publishes or updates the draft GitHub Release assets.
1. The release issue is closed.
1. An announcement email is sent to `dev@kubernetes.io` with the subject `[ANNOUNCE] agent-sandbox $VERSION is released`.

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
