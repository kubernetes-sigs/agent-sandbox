# TypeScript Client SDK for Agent Sandbox

This TypeScript client provides a high-level interface for creating and interacting with sandboxes managed by the Agent Sandbox controller, mirroring the [Go client](../../go/README.md) and [Python client](../../python/agentic-sandbox-client/README.md).

The surface covers the Kubernetes resource layer (provisioning a `SandboxClaim`, watching it to readiness, and tearing it down via `SandboxClient` / `Sandbox`) and the sandboxd runtime layer (`sandbox.commands.run()`, `sandbox.files.{read,write,readStream,writeStream,exists,list,delete}()`, `sandbox.health()`, and `sandbox.metadata()`). `Start`/PTY/interactive process support is not part of this surface yet.

## Usage

```ts
import { SandboxClient } from "agentic-sandbox-client";

const client = new SandboxClient({ namespace: "default" });
const sandbox = await client.createSandbox("my-warmpool");

try {
  // Connects to sandboxd lazily on first use; both facades share one
  // connection once established.
  const result = await sandbox.commands.run("echo", ["hello"]);
  console.log(result.stdout, result.exitCode);

  // No shell is involved; invoke one explicitly for pipes, redirects, etc.
  await sandbox.commands.run("sh", ["-c", "ls | wc -l"], {
    env: { LC_ALL: "C" },
    cwd: "work",
  });

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
- `sandboxd.portForwardReadyTimeoutMs` (constructor option, under `sandboxd`) is in **milliseconds** and bounds the shared connection to sandboxd, through a successful health check: for `port-forward` connectivity it starts when both local port-forward listeners are opened, and for the in-cluster modes it bounds health polling against the pod address (see [Connectivity](#connectivity)). It applies in full to every (re)connect attempt, including reconnects after a transport failure. Default: 30000.
- Every `sandbox.files.*` / `sandbox.commands.run()` / `sandbox.health()` / `sandbox.metadata()` call takes a per-call `timeoutMs` (default 60000). This is a total budget for the call, including any time spent waiting on the shared connection above — a cold first call can spend most of its budget just connecting.
- `sandboxd.maxCommandOutputSize` bounds the fully-decoded `ExecuteResponse` (stdout + stderr + protobuf framing combined, not stdout alone) that `sandbox.commands.run()` will accept.

### Connectivity

`sandboxd.connectivity` selects how the SDK reaches sandboxd. The values are the same as the Go client's `Connectivity`:

| Value | Path | Requirements |
| --- | --- | --- |
| `"port-forward"` (default) | WebSocket port-forward brokered by the apiserver | A kubeconfig that allows `pods/portforward`. Works from anywhere, including a laptop or CI runner. |
| `"in-cluster-service"` | Dials the Sandbox's headless Service by DNS name (`status.serviceFQDN`) | The process runs inside the cluster, and the template sets `spec.service: true`. |
| `"in-cluster-pod-ip"` | Dials the pod IP (`status.podIPs`, IPv4 preferred) | The process runs inside the cluster. |

```ts
const client = new SandboxClient({
  sandboxd: { connectivity: "in-cluster-service" },
});
```

The in-cluster modes take the apiserver off the data path. Each uses exactly one address, and neither falls back to the other or to port-forwarding:

- With `"in-cluster-service"`, `createSandbox()` / `getSandbox()` throw `SandboxNoServiceError` when the Sandbox has no Service, instead of falling back to the pod IP. Prefer this mode when sandboxes cross a trust boundary: the Service only selects its own Sandbox's pod, and deleting the Sandbox deletes the Service, so connections fail instead of reaching another pod that inherited the IP. DNS caching still leaves a TTL-bounded window.
- With `"in-cluster-pod-ip"`, `createSandbox()` / `getSandbox()` throw `SandboxMetadataError` when the Sandbox reports no pod IP. This mode needs no template change, but nothing detects that the pod was rescheduled: a handle keeps dialing the address it saw when it was created, which Kubernetes may since have reassigned to an unrelated pod.

The addresses are available on the handle as `sandbox.podIP` and `sandbox.serviceFQDN`. They are `""` when unknown.

### Streaming

`sandbox.files.readStream()` / `writeStream()` transfer a file without buffering the whole payload in memory, unlike `read()`/`write()`:

```ts
import { createReadStream, createWriteStream } from "node:fs";
import { Readable, Writable } from "node:stream";

// Upload a large local file without buffering it.
await sandbox.files.writeStream(
  "data/input.bin",
  Readable.toWeb(createReadStream("./input.bin")) as ReadableStream<Uint8Array>,
  { timeoutMs: 10 * 60_000 },
);

// Download and stream straight to disk.
const stream = await sandbox.files.readStream("data/output.bin", {
  timeoutMs: 10 * 60_000,
});
await stream.pipeTo(Writable.toWeb(createWriteStream("./output.bin")));
```

A few things differ from the buffered methods:

- **Ownership**: `readStream()` resolves once the response headers are validated, before the file is fully downloaded — you must read the returned `ReadableStream` to completion or cancel it (e.g. `reader.cancel()`). An abandoned, unread stream keeps its operation in flight until `opts.timeoutMs` elapses or the sandbox is closed. `writeStream()` consumes its input stream at most once; the SDK cancels it on any termination after consumption starts (timeout, abort, an oversized upload, a non-204 response), but a validation failure that happens *before* the stream is touched (an invalid mode, or content that's already locked by another reader) leaves the stream with you.
- **Timeouts**: `opts.timeoutMs` (same option as the buffered methods, default 60000) is a total budget from the call through full consumption. For `readStream()` it keeps running even while you're not reading; for `writeStream()` it includes time spent waiting on your input stream. A timeout errors the stream/promise and releases the operation without waiting for another read or write.
- **Size limits**: `sandboxd.maxDownloadSize` / `sandboxd.maxUploadSize` are enforced incrementally as bytes arrive or are sent, not against a `Content-Length` header — an oversized transfer is aborted mid-stream rather than buffered in full and then rejected. On an oversized upload, sandboxd may already have received a prefix within the limit before the abort; its atomic rename (see "No automatic retry" below) still keeps that prefix from ever becoming the target file's visible content.
- **`close()`**: an unconsumed `readStream()` or an in-flight `writeStream()` counts toward `close()`'s in-flight drain, bounded by the same cleanup timeout as every other operation.

### Health and metadata

`sandbox.health()` and `sandbox.metadata()` take the same per-call options as the other runtime calls (`RuntimeCallOptions`: `timeoutMs` and `signal`). `FileCallOptions` is kept as an alias of it.

```ts
const { status, uptimeSeconds } = await sandbox.health();
const { env } = await sandbox.metadata();
console.log(env.SANDBOX_ID);
```

- `health()` probes sandboxd's `/v1/health` and resolves with `{ status: "ok", uptimeSeconds }`. The lazy connect already waits for sandboxd to become healthy, so a first call against a not-yet-ready sandboxd fails with the connect's timeout or connection error, not a 503; only on an already-established connection does an unready sandboxd (for example during shutdown) reject with a `SandboxdApiError` (status 503). A resolved `health()` means "reachable and ready now", not "just came up".
- `metadata()` reads sandboxd's `/v1/metadata` and resolves with `{ env }`: the orchestrator-injected variables sandboxd chooses to expose. sandboxd serves only names starting with its `--metadata-env-prefix` (default `SANDBOX_`) and withholds any name that looks like a credential (containing `TOKEN`, `SECRET`, `KEY`, and similar). `env` is therefore not sandboxd's full environment, and is empty when nothing matches. Templates that need a value visible here must set it on the sandboxd container under the configured prefix; nothing in the controller injects one for you.
- Neither call is retried automatically, and neither records any value in tracing spans (`metadata()` records only the number of variables).

### Execution target and path rules

`sandbox.commands.run(command, args?, options?)` (or `run(command, options?)`) passes `[command, ...args]` to sandboxd as the process's argv, exactly like `ProcessConfig.command` — it is never wrapped in a shell, so there is no word splitting, globbing, or variable expansion. Use `run("sh", ["-c", "..."])` when you need shell syntax. Because there is no shell in between, an executable that cannot be found is reported by sandboxd as a `SandboxdRpcError` with code `not_found`, not as a result with exit code 127.

- `options.env` is merged over sandboxd's own environment (it does not replace it). `PATH` set here does not change how `command` itself is looked up — sandboxd resolves it against its own `PATH` — so pass an absolute path if you need a specific executable.
- `options.cwd` is resolved relative to the sandbox root (default: the sandbox root) by sandboxd, with the same symlink-aware confinement as the files API; a directory outside the sandbox root is rejected with a `SandboxdRpcError` (code `permission_denied`).

`run()` executes inside the container running sandboxd. If your Pod spec's sandboxd container has a different root filesystem than a "workload" sidecar container, `run()` only ever executes in the sandboxd container — a shared volume does not make binaries from another container available to it.

All file paths are sandbox-root-relative POSIX paths and are validated **before any network request**, without being decoded or normalized first:

- `""` and `"."` (and equivalent all-dot/empty-segment forms) refer to the sandbox root. `read`/`list`/`exists` accept it; `write`/`delete` reject it, as they do a trailing `/`.
- An absolute path (leading `/`) is always rejected.
- Any `..` path segment is always rejected, including for `exists()` (which never silently reports `false` for a rejected path — it throws).

This path confinement covers the files API and the working directory sandboxd runs commands from; it does not by itself isolate the command's process tree, filesystem, or network access — that is the responsibility of the Pod's `runtimeClassName`, `securityContext`, volumes, `NetworkPolicy`, and RBAC.

### No automatic retry

A failed `files.*`, `commands.run()`, `health()` or `metadata()` call is never retried automatically by the SDK. If the underlying connection was invalidated by a transport failure, the *next* call you make reconnects from scratch; other calls already in flight on the same connection fail together with it.

What "retrying" safely means differs per method:

| Method | Automatic retry | Retrying from scratch | Constraints |
| --- | --- | --- | --- |
| `run()` | No | Re-run the command | A failed call may or may not have executed to completion server-side before the failure was observed — retrying blindly could re-run a command that already had side effects. |
| `health()` / `metadata()` | No | Call it again | Read-only; the result reflects sandboxd at the time of the call. |
| `read()` | No | Call it again | The file may have changed between attempts. |
| `write()` | No | Resend the same `content` | sandboxd replaces the target atomically via a temporary file and rename, so a failed call never leaves the target partially written. But if the acknowledgement was lost, the write may already have committed server-side, and a retry can overwrite a concurrent update from someone else. |
| `readStream()` | No | Call it again from the beginning | Discard any partial output and reset your destination first — a partial download is never resumed. The file may have changed between attempts. |
| `writeStream()` | No | Provide a fresh stream producing the same content | The input stream is consumed at most once, so retrying needs a new stream instance, not the same one rewound. Same atomicity/acknowledgement/concurrent-update caveats as `write()`. |

The atomicity guarantee — the target path never briefly shows partial content — is independent of whether *you* can tell a given call actually completed server-side, and of whether a retry might overwrite someone else's concurrent write.

### Trust boundary

With `port-forward` connectivity, the local TCP listeners the SDK opens for its port-forward tunnel (`127.0.0.1`, random ports) have no authentication of their own. Any other process in the same network namespace can reach the sandbox's files/run API through them for as long as the connection is open. Treat other local processes as trusted, the same way you would for any other unauthenticated `localhost` service.

With the in-cluster modes, REST and gRPC travel in plaintext across the pod network, with no authentication. Anything that can reach the sandbox pod on those ports can use its files/run API. Restrict that access with a NetworkPolicy or a service mesh.

### Differences from the Go and Python clients

- `run()` takes an argv (`command`, `args`) mirroring sandboxd's `ProcessConfig`, plus `env` and `cwd`; the Go/Python clients take a single shell string that they wrap in `/bin/sh -c`.
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
