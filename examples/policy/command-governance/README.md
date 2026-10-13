# Command Governance

The [`network-policy-management`](../network-policy-management), [`vap`](../vap),
[`opa-gatekeeper`](../opa-gatekeeper), and [`kyverno`](../kyverno) examples in
this directory govern Kubernetes-level concerns: what a `Sandbox` object is
allowed to look like, and what its pod can reach over the network. None of
them see the content of a command dispatched to a running sandbox's
execution API — a `sandbox.commands.run("rm -rf /")` call is indistinguishable
from any other command at the L3/L4/admission layer.

This example governs that layer instead: the driving script classifies and
approves/denies a command *before* calling `sandbox.commands.run()`, so a
denied command never reaches the pod at all.

## Prerequisites

Same as [python-sdk-quickstart](../../python-sdk-quickstart): a cluster with
the controller, router, and a `python-sandbox-pool` `SandboxWarmPool`
running, plus `pip install k8s-agent-sandbox`.

## Usage

The classification logic lives in [`governed_run.py`](governed_run.py) (unit
tests in [`test_governed_run.py`](test_governed_run.py)) rather than inline
here, because it does real parsing — shell chaining/piping, command
substitution, `sudo`/`env`/`xargs` wrapper stripping — that's worth
regression-testing, not just illustrating.

```python
from governed_run import governed_run
from k8s_agent_sandbox import SandboxClient

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")
try:
    print(governed_run(sandbox, "echo hello").stdout)
    # hello

    governed_run(sandbox, "rm -rf /")
    # PermissionError: denied by command policy: rm -rf /

    governed_run(sandbox, "echo ok && rm -rf /")
    # PermissionError: denied by command policy: echo ok && rm -rf /
finally:
    sandbox.terminate()
```

`rm -rf /` is rejected in this script's own process — the sandbox pod is
never contacted for that call. This is a minimal illustration; swap
`is_denied()` for whatever policy engine (OPA, a YAML rules file, an LLM
classifier) fits your risk model — the wrapper shape around
`sandbox.commands.run()` is the actual pattern, not the matching logic.

**Scope:** `is_denied()` checks the raw command text for destructive syntax
(fork bombs, `curl|sh` installers), then splits on shell chaining/piping
metacharacters (`;`, `&&`, `||`, `|`, `&`) and command substitution
(`$(...)`, backticks — including nested), and runs a token-aware `rm`/
`mkfs`/`dd` check against each resulting segment independently. That
catches direct invocations (quoted/escaped spellings, path-qualified
executables like `/bin/rm`), chaining (`echo hi; rm -rf /`), substitution
(`$(rm -rf /)`), and `sudo`/`env`/`xargs`/`command`/`nohup`/`nice`/`stdbuf`/
`timeout`/`exec` wrappers (`sudo rm -rf /`, including each wrapper's own
arg-taking flags like `sudo --user root`, and `timeout`'s mandatory
`DURATION` positional) — see `test_governed_run.py` for the full case list. A
command in the executable position that resolves via shell expansion, a
glob, or brace expansion rather than being a literal spelling
(`$(printf rm) --recursive --force /`, `` `printf rm` --recursive --force / ``,
`/bin/r[m] -rf /`, IFS-glued tokens like `rm$IFS-rf$IFS/`, or
`{rm,-rf,/}` where brace expansion turns one token into the three words
`rm -rf /`) is denied outright rather than evaluated, since correctly
resolving it would mean being a shell - and the same applies to any
*argument* of an `rm`/`mkfs`/`dd` invocation, not just the executable
token, since a hidden expansion there (`FLAGS=-rf; rm $FLAGS /`) could
resolve to a destructive flag the literal-text check never sees. Because
`xargs` appends
argv tokens it reads from its own *stdin* - typically the previous stage of
a pipeline, a separate segment this checker can't see - to the command it
wraps, any `xargs` invocation targeting `rm`/`mkfs`/`dd` is denied
unconditionally regardless of what flags are visible on the `xargs` line
itself (`find / | xargs rm` is denied even though no `-rf` appears anywhere
in the text). It is **not** a general
shell-grammar parser: constructs it doesn't specifically split on or
unwrap (arbitrary subshell forms, `eval`, `bash -c '...'`, redirections,
control-flow bodies, a command embedded in another language's string
literal) can still slip through, because each of those requires its own
grammar to unwrap rather than a fixed set of split characters. Closing
that gap fully means either parsing the command as a full shell AST or,
more robustly, allowlisting the exact commands a sandbox is permitted to
run instead of denylisting patterns — denylists are inherently a losing
game against a determined adversary. Pick allowlisting for anything
beyond a demo.
