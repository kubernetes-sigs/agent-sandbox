# Coding agents with `agtsbx run`

Coding agents edit files and run commands on your behalf, which is exactly the
work a sandbox exists to contain. [`agtsbx`](../../cmd/agtsbx/README.md) runs
one in a throwaway sandbox and streams the result back, so nothing the agent
does touches the host:

```console
$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest \
    claude -p 'write hello.py, then run it'
```

All three agents packaged here have a headless mode, which is what makes them
a fit for a one-shot `agtsbx run`.

## Build the image

The image is a [sandboxd](../../packages/sandboxd/USER_GUIDE.md) runtime
image, the KEP-539.2 daemon as its entrypoint, with the agent CLIs added:

```console
$ docker build -t agtsbx-agents:latest examples/agtsbx-agents
```

| Agent | Package | Headless invocation |
| --- | --- | --- |
| [opencode](https://opencode.ai) | `opencode-ai` | `opencode run PROMPT` |
| [Claude Code](https://docs.anthropic.com/en/docs/claude-code) | `@anthropic-ai/claude-code` | `claude -p PROMPT` |
| [Codex](https://developers.openai.com/codex/cli) | `@openai/codex` | `codex exec PROMPT` |

## Run

Each agent needs its provider credential, passed with `-e`:

```console
$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest \
    claude -p 'summarise the files in this directory'

$ agtsbx run -e OPENAI_API_KEY agtsbx-agents:latest \
    codex exec 'write a fizzbuzz in Rust'

$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest \
    opencode run 'add a docstring to every function in main.py'
```

`-e KEY` with no value forwards the variable from your own environment, so the
credential stays out of the command line and the shell history. Only the
variables named this way reach the agent, and they travel with the command
rather than being stored on the sandbox.

Agents stop to ask permission before acting, which a one-shot run cannot
answer; each has a flag to skip that (`claude --permission-mode`, `codex exec
--dangerously-bypass-approvals-and-sandbox`, `opencode run --agent build`).
Those flags are far safer here than on a workstation, because the sandbox is
the boundary and it is discarded when the command exits.

`--runtime kubernetes` runs the same agent as a `Sandbox` object in a cluster,
with no other change to the command:

```console
$ agtsbx run --runtime kubernetes -n agents -e ANTHROPIC_API_KEY \
    agtsbx-agents:latest claude -p 'what is in this workspace?'
```

## Giving the agent something to work on

The sandbox starts empty, so clone the repository first and chain the agent
onto the same command:

```console
$ agtsbx run -e ANTHROPIC_API_KEY agtsbx-agents:latest sh -c '
    git clone --depth 1 https://github.com/kubernetes-sigs/agent-sandbox repo &&
    cd repo && claude -p "what does the Sandbox controller reconcile?"'
```

For work you want to keep, `--keep` leaves the sandbox in place so the results
can be copied out:

```console
$ agtsbx run --keep --name agentbox -e ANTHROPIC_API_KEY agtsbx-agents:latest \
    claude -p 'write hello.py'
$ docker cp agentbox:/workspace/hello.py .
$ docker rm -f agentbox
```

## Notes

- The agents are unpinned; add versions to the `npm install` line in the
  [Dockerfile](Dockerfile) if you need reproducible runs.
- The sandbox has unrestricted egress, which the agents need in order to reach
  their model APIs. To narrow that to the provider alone, run under
  `--runtime kubernetes` and attach a NetworkPolicy, see
  [`examples/composing-sandbox-nw-policies`](../composing-sandbox-nw-policies).
