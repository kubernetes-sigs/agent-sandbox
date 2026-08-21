# TypeScript Client SDK for Agent Sandbox

This TypeScript client provides a high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller, mirroring the [Go client](../../go/README.md) and [Python client](../../python/agentic-sandbox-client/README.md).

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
