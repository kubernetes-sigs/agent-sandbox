# TypeScript Client SDK for Agent Sandbox

This TypeScript client provides a high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller, mirroring the [Go client](../../go/README.md) and [Python client](../../python/agentic-sandbox-client/README.md).

The surface covers the Kubernetes resource layer (provisioning a `SandboxClaim`, watching it to readiness, and tearing it down via `SandboxClient` / `Sandbox`) and the sandboxd runtime layer (`sandbox.commands.run()` and `sandbox.files.{read,write,exists,list,delete}()`). `Start`/PTY/interactive process support is not part of this surface yet.

## Usage

```ts
import { SandboxClient } from "agentic-sandbox-client";

const client = new SandboxClient({ namespace: "default" });
const sandbox = await client.createSandbox("my-warmpool");

try {
  // Connects to sandboxd lazily on first use; both facades share one
  // connection once established.
  const result = await sandbox.commands.run("echo hello");
  console.log(result.stdout, result.exitCode);

  await sandbox.files.write("greeting.txt", "hello from the SDK\n");
  const contents = await sandbox.files.read("greeting.txt");
  console.log(new TextDecoder().decode(contents));

  const listing = await sandbox.files.list(".");
  console.log(listing.entries.map((e) => e.name));
} finally {
  await sandbox.close();
}
```

### Timeouts

- `sandboxReadyTimeout` (constructor / `createSandbox()` options) is in **seconds** and bounds waiting for the `SandboxClaim`/`Sandbox` to become `Ready`. Default: 180.
- `sandboxd.portForwardReadyTimeoutMs` (constructor option, under `sandboxd`) is in **milliseconds** and bounds the shared connection to sandboxd — opening both local port-forward listeners through a successful health check. It applies in full to every (re)connect attempt, including reconnects after a transport failure. Default: 30000.
- Every `sandbox.files.*` / `sandbox.commands.run()` call takes a per-call `timeoutMs` (default 60000). This is a total budget for the call, including any time spent waiting on the shared connection above — a cold first call can spend most of its budget just connecting.
- `sandboxd.maxCommandOutputSize` bounds the fully-decoded `ExecuteResponse` (stdout + stderr + protobuf framing combined, not stdout alone) that `sandbox.commands.run()` will accept.

### Execution target and path rules

`sandbox.commands.run(command)` always executes `/bin/sh -c <command>` inside the container running sandboxd. If your Pod spec's sandboxd container has a different root filesystem than a "workload" sidecar container, `run()` only ever executes in the sandboxd container — a shared volume does not make binaries from another container available to it.

All file paths are sandbox-root-relative POSIX paths and are validated **before any network request**, without being decoded or normalized first:

- `""` and `"."` (and equivalent all-dot/empty-segment forms) refer to the sandbox root. `read`/`list`/`exists` accept it; `write`/`delete` reject it, as they do a trailing `/`.
- An absolute path (leading `/`) is always rejected.
- Any `..` path segment is always rejected, including for `exists()` (which never silently reports `false` for a rejected path — it throws).

This path confinement covers the files API and the working directory sandboxd runs commands from; it does not by itself isolate the command's process tree, filesystem, or network access — that is the responsibility of the Pod's `runtimeClassName`, `securityContext`, volumes, `NetworkPolicy`, and RBAC.

### No automatic retry

A failed `files.*` or `commands.run()` call is never retried automatically by the SDK. In particular, a failed `run()` call may or may not have executed to completion server-side before the failure was observed — retrying blindly could re-run a command that already had side effects. If the underlying connection was invalidated by a transport failure, the *next* call you make reconnects from scratch; other calls already in flight on the same connection fail together with it.

### Trust boundary

The local TCP listeners the SDK opens for its port-forward tunnel (`127.0.0.1`, random ports) have no authentication of their own. Any other process in the same network namespace can reach the sandbox's files/run API through them for as long as the connection is open. Treat other local processes as trusted, the same way you would for any other unauthenticated `localhost` service.

### Differences from the Go and Python clients

- `run()` always executes via `/bin/sh -c` — there is no way to set the executable directly, unlike the Go/Python clients' argv-style APIs.
- File paths are never recorded in tracing spans or logs (only counts/sizes/booleans are), and absolute paths / `..` segments are rejected by the client itself before any request is sent.
- RuntimeClass (gVisor/Kata) is not observed or branched on anywhere in this layer; conformance on non-default runtimes is tracked separately and is not implied by this SDK's tests passing on a standard cluster.

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

`sandbox.commands.run()` additionally requires the optional `@bufbuild/protobuf`, `@connectrpc/connect`, and `@connectrpc/connect-node` peer dependencies (declared as optional peers in [package.json](package.json)). They are loaded lazily on first use, so `sandbox.files.*` and everything else in the package works without them installed; calling `run()` without them throws a clear error naming the packages to install.

See [src/index.ts](src/index.ts) for the full set of exports.

## Automatic expiration

Set `shutdownAfterSeconds` when creating a sandbox to have the controller delete
its claim and sandbox after the requested lifetime, even if the client process
exits before cleaning up:

```typescript
import { SandboxClient } from "./dist/index.js";

const client = new SandboxClient();
const sandbox = await client.createSandbox("my-warm-pool", "default", {
  shutdownAfterSeconds: 300,
});
```

The lifetime starts at the `createSandbox` call and includes provisioning time.
The option sets an absolute `spec.lifecycle.shutdownTime` and
`spec.lifecycle.shutdownPolicy: "Delete"` on the claim, matching the Python SDK's
`shutdown_after_seconds`. It must be a positive integer that produces a valid
RFC3339 deadline; invalid values reject with `SandboxError` before provisioning.
Omitting it leaves expiration unset. Continue to call `sandbox.close()` when work
finishes; expiration provides a fallback if the client cannot clean up.

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
