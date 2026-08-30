# agtsbx

`agtsbx` is a docker-like command-line front end for agent-sandbox. It runs a
single command in a throwaway sandbox and streams the output back:

```console
$ agtsbx run sandboxd:latest echo hello
agtsbx: starting sandboxd:latest on docker as agtsbx-1b75a73862a3
hello
```

The sandbox is created, the command runs, and the sandbox is torn down. The
exit status of `agtsbx run` is the exit status of the command, so it composes
in shell pipelines the way `docker run` does.

## Why

Trying agent-sandbox has meant standing up a cluster, installing the
controller and writing a `SandboxTemplate` before running anything. That is a
lot of ceremony for "does my agent's code work in a sandbox?". `agtsbx run`
removes it for the local case while keeping the same runtime semantics as the
cluster case.

## Runtimes

The sandbox can run on a local container engine or in a Kubernetes cluster.
`--runtime` selects; the default (`auto`) probes for docker, then podman, then
falls back to kubernetes.

| `--runtime` | What it creates | Needs |
| --- | --- | --- |
| `docker` | a local container | the `docker` CLI |
| `podman` | a local container | the `podman` CLI |
| `kubernetes` | a `Sandbox` object | a kubeconfig and the agent-sandbox controller |

An explicit `--runtime` is never silently downgraded: asking for `podman` on a
host without podman is an error, not a fallback to docker.

All three converge on the same in-sandbox contract, so a command behaves the
same wherever it ran:

```console
$ agtsbx run --runtime docker     sandboxd:latest sh -c 'echo $((6*7))'
42
$ agtsbx run --runtime kubernetes sandboxd:latest sh -c 'echo $((6*7))'
42
```

## The image contract

`IMAGE` must start [`sandboxd`](../../packages/sandboxd/USER_GUIDE.md), the
portable runtime daemon defined by
[KEP-539.2](../../docs/keps/539.2-runtime-standardization/README.md), as its
entrypoint. `agtsbx` does not exec into the container; it talks to sandboxd's
`ProcessService`, which is what makes the behaviour identical across backends.

`packages/sandboxd/Dockerfile` builds a suitable base image, and
`examples/sandboxd-sandbox/` shows how to add tools to it:

```console
$ docker build -f packages/sandboxd/Dockerfile -t sandboxd:local .
$ agtsbx run sandboxd:local sh -c 'echo ready'
```

A command is required. Running the image with no command would just start the
daemon and appear to hang, so `agtsbx` asks for one up front.

## Usage

```text
agtsbx run [OPTIONS] IMAGE COMMAND [ARG...]
```

| Option | Default | Description |
| --- | --- | --- |
| `--runtime` | `auto` | `auto`, `docker`, `podman` or `kubernetes` |
| `-e`, `--env KEY=VALUE` | | Environment variable for the command; `-e KEY` forwards it from the current environment (repeatable) |
| `-w`, `--workdir` | sandbox root | Working directory for the command |
| `--name` | generated | Name for the sandbox |
| `-n`, `--namespace` | `default` | Namespace, for the `kubernetes` runtime |
| `--keep` | `false` | Leave the sandbox running after the command exits |
| `--timeout` | `3m` | How long to wait for the sandbox to become ready |
| `--grpc-port` | `9090` | sandboxd `ProcessService` port inside the sandbox |
| `--rest-port` | `8080` | sandboxd REST API port inside the sandbox |
| `-q`, `--quiet` | `false` | Suppress progress messages on stderr |

Everything after `IMAGE` is passed to the sandboxed command untouched, so its
own flags need no escaping:

```console
$ agtsbx run sandboxd:local ls -la /workspace
```

Progress messages go to stderr and the command's output to stdout, so stdout
stays clean for piping:

```console
$ agtsbx run -q sandboxd:local cat /etc/os-release | grep ^ID
ID=debian
```

### `--workdir` is inside the sandbox

sandboxd confines the working directory to the sandbox root (`/workspace` by
default), so `--workdir` is resolved there and must already exist. Passing a
host path such as `/tmp` resolves to `/workspace/tmp`, which usually does not
exist; `agtsbx` detects that case and says so rather than letting the
underlying error blame the command binary.

## Coding agents

A coding agent edits files and runs commands on your behalf, which is the work
a sandbox exists to contain, and all three of the popular CLIs have a headless
mode that fits a one-shot `agtsbx run`.
[`examples/agtsbx-agents`](../../examples/agtsbx-agents) builds an image with
them installed:

```console
$ docker build -t agtsbx-agents:latest examples/agtsbx-agents
$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest claude -p 'write hello.py'
$ agtsbx run -e OPENAI_API_KEY    agtsbx-agents:latest codex exec 'write hello.py'
$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest opencode run 'write hello.py'
```

`-e KEY` with no value forwards the variable from your own environment, so the
credential stays out of the command line and the shell history. Variables are
sent with the command itself, so they are not stored on the container or in
the `Sandbox` object.

## Examples

```console
# Environment variables
$ agtsbx run -q -e GREETING=hi sandboxd:local sh -c 'echo $GREETING'
hi

# Exit statuses propagate
$ agtsbx run -q sandboxd:local sh -c 'exit 42'; echo $?
42

# Output streams as it is produced, not buffered until exit
$ agtsbx run -q sandboxd:local sh -c 'for i in 1 2 3; do echo $i; sleep 1; done'

# Keep the sandbox for inspection
$ agtsbx run --keep --name debugbox sandboxd:local echo started
$ docker exec -it debugbox sh
```

## Cleanup

The sandbox is removed when the command exits, including when it fails and
when `agtsbx` is interrupted with Ctrl-C. A `Start` that fails part-way tears
down whatever it had already created, so a failed run leaves nothing behind.
`--keep` opts out and leaves the sandbox running.

## Security

The sandbox is meant to run untrusted code, and sandboxd performs no
authentication of its own: KEP-539.2 places containment in the network layer.
`agtsbx` therefore:

- publishes container ports on `127.0.0.1` only, never on all interfaces, so
  the sandbox control plane is not reachable off-host;
- reaches Kubernetes sandboxes over a port-forward, so nothing is exposed
  cluster-wide and no `Service`, `Gateway` or router is required;
- creates Kubernetes sandboxes with `automountServiceAccountToken: false`,
  `runAsNonRoot`, `allowPrivilegeEscalation: false` and all capabilities
  dropped, matching `examples/sandboxd-sandbox/sandbox-template.yaml`;
- runs containers with `--cap-drop ALL` and `--security-opt
  no-new-privileges`;
- sends the command's environment with the command, so credentials passed
  with `-e` are not written to the engine's argv or the `Sandbox` object.

Local engines have no `runAsNonRoot` equivalent, so on that path the image
decides which user the command runs as; the `kubernetes` runtime rejects an
image that would run as root.

## Building

```console
$ make build-agtsbx   # -> bin/agtsbx
```
