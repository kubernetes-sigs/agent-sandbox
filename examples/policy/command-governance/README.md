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

```python
import os
import shlex
from k8s_agent_sandbox import SandboxClient

def _flags(tokens: list[str]) -> list[str]:
    # Tokens up to (not including) a bare "--" end-of-options marker, so
    # `rm -- --recursive --force` (real filenames, not flags) isn't denied.
    if "--" in tokens:
        return tokens[: tokens.index("--")]
    return tokens

def _has_flag(tokens: list[str], short: str, long_name: str) -> bool:
    # Checks parsed argv tokens, not the raw string, so quoting/escaping
    # ("-r -f", "'-rf'") can't hide a flag the shell would still honor.
    for t in tokens:
        if t == long_name:
            return True
        if t.startswith("-") and not t.startswith("--") and short.lower() in t.lower():
            return True
    return False

def _is_denied(command: str) -> bool:
    try:
        tokens = shlex.split(command)  # parses quoting the way a POSIX shell would
    except ValueError:
        return True  # unparseable quoting - fail safe, deny
    if not tokens:
        return False
    exe = os.path.basename(tokens[0])  # strips a path prefix like /bin/rm
    args = _flags(tokens[1:])
    if exe == "rm":
        return _has_flag(args, "r", "--recursive") and _has_flag(args, "f", "--force")
    if exe == "mkfs" or exe.startswith("mkfs."):
        return True
    if exe == "dd":
        return any(t.startswith("of=") for t in tokens[1:])  # order-independent
    return False

def governed_run(sandbox, command: str):
    if _is_denied(command):
        raise PermissionError(f"denied by command policy: {command}")
    return sandbox.commands.run(command)

client = SandboxClient()
sandbox = client.create_sandbox(warmpool="python-sandbox-pool", namespace="default")
try:
    print(governed_run(sandbox, "echo hello").stdout)
    # hello

    governed_run(sandbox, "rm -rf /")
    # PermissionError: denied by command policy: rm -rf /
finally:
    sandbox.terminate()
```

`rm -rf /` is rejected in this script's own process — the sandbox pod is
never contacted for that call. This is a minimal illustration; swap
`_is_denied()` for whatever policy engine (OPA, a YAML rules file, an LLM
classifier) fits your risk model — the wrapper shape around
`sandbox.commands.run()` is the actual pattern, not the matching logic.

**Scope:** `_is_denied()` only inspects the first parsed token (the
executable) and its own flags/arguments, so it catches `rm`/`mkfs`/`dd`
invoked directly (including quoted/escaped spellings and path-qualified
executables like `/bin/rm`) but not one reached via shell chaining or
substitution (`echo hi; rm -rf /`, `$(rm -rf /)`, pipes). Closing that fully
means parsing the command as a
shell script (not just a word list) or, more robustly, allowlisting the
exact commands a sandbox is permitted to run instead of denylisting
patterns — denylists are inherently a losing game against a determined
adversary. Pick allowlisting for anything beyond a demo.
