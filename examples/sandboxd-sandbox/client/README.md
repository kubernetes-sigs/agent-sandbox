# sandboxd SDK example (Go)

A minimal end-to-end program that uses the Go SDK against the **sandboxd**
runtime. It:

1. creates a sandbox from a warm pool,
2. writes a file into `/workspace` over the REST filesystem,
3. execs a command over the gRPC `ProcessService`,
4. reads the file back and prints the command output,

then deletes the sandbox on exit.

## Prerequisites

- A Kubernetes cluster you can reach with `kubectl` (a `kind` cluster is fine).
- A sandboxd-backed `SandboxTemplate` **and** `SandboxWarmPool` applied to the
  cluster. Use [`../sandbox-template.yaml`](../sandbox-template.yaml) as the
  template (it runs the `sandboxd` image), and create a `SandboxWarmPool` that
  references it.
- Your kubeconfig must allow **port-forward to pods** in the target namespace —
  the SDK reaches sandboxd via a pod port-forward.

## Run

```console
$ go run ./examples/sandboxd-sandbox/client \
    -warmpool sandboxd-warmpool \
    -namespace default
```

Flags:

| flag | default | description |
|------|---------|-------------|
| `-warmpool`  | `sandboxd-warmpool` | SandboxWarmPool to claim from |
| `-namespace` | `default`           | Namespace of the warm pool / sandbox |
| `-file`      | `greeting.txt`      | Path (relative to `/workspace`) to write and read back |
| `-content`   | `hello from …`      | Content to write |

## Expected output

```
created sandbox "sandboxd-xxxxx" (claim "sandboxd-warmpool-xxxxx")
wrote 36 bytes to greeting.txt
run: exit=0
stdout: hello from the sandboxd SDK example
read back 36 bytes, content matches
```

## Notes

- Commands run through `ProcessService` execute **inside the `sandboxd`
  container**, with `/workspace` as the working directory. Files written over
  REST land in that same shared volume, which is why `cat greeting.txt` sees the
  file the SDK just wrote.
- The base `sandboxd` image ships a minimal (busybox) toolset. To run a language
  runtime (e.g. `python3`), build a sandboxd image that includes it.
