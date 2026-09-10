# TypeScript Client SDK for Agent Sandbox

This TypeScript client provides a high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller, mirroring the [Go client](../../go/README.md) and [Python client](../../python/agentic-sandbox-client/README.md).

The current surface covers the Kubernetes resource layer only: provisioning a `SandboxClaim`, watching it to readiness, and tearing it down (`SandboxClient` / `Sandbox`). Connectivity to the sandbox runtime (running commands, reading and writing files) lands as a follow-up — see [issue #977](https://github.com/kubernetes-sigs/agent-sandbox/issues/977).

## Publishing status

This package is **not currently distributed** in any form — there is no npm package and no git-based distribution channel today.
`"private": true` in [package.json](package.json) is set intentionally, as a safeguard against accidentally publishing an unfinished package to the npm registry (e.g. via a stray `npm publish` or an automated release step).
It is still under active development and its public API may change without notice.

## Development / local usage

Until this package is published, use it by checking out this repository and building it locally
from this directory:

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox.git
cd agent-sandbox/clients/typescript/agentic-sandbox-client
npm install
npm run build
```

See [src/index.ts](src/index.ts) for the full set of exports.

## Listing sandboxes

With Kubernetes credentials configured and permission to list SandboxClaims, run
the following from this package directory after building locally:

```ts
import { SandboxClient } from "./dist/index.js";

const client = new SandboxClient({ namespace: "default" });

const allClaims = await client.listAllSandboxes("default");
const appClaims = await client.listAllSandboxes("default", "app=my-agent");
const devClaims = await client.listAllSandboxes(
  undefined,
  "env in (dev,test),!disabled",
);
```

The optional second argument is a Kubernetes label selector for
`SandboxClaim.metadata.labels` (set through `createSandbox`'s `labels` option),
not Pod labels. Omitting the selector or passing an empty string lists all claims
in the namespace. Omitting the namespace or passing `undefined` or an empty string
uses the client's configured default namespace.
