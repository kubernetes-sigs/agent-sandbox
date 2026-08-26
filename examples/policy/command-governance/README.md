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
import re
from k8s_agent_sandbox import SandboxClient

_DENY_PATTERNS = [
    r"\bmkfs\b",
    r"\bdd\s+if=",
]

def _has_flag(command: str, short: str, long_name: str) -> bool:
    # Matches the short flag standalone or combined with others (-rf, -fr,
    # -Rf), and the long GNU option, so "-r -f", "-fr", and "--recursive
    # --force" are all treated the same regardless of order.
    return bool(re.search(rf"(?:^|\s)-\w*{short}\w*(?:\s|$)", command, re.IGNORECASE)) or long_name in command

def _is_destructive_rm(command: str) -> bool:
    return (
        bool(re.search(r"\brm\b", command, re.IGNORECASE))
        and _has_flag(command, "r", "--recursive")
        and _has_flag(command, "f", "--force")
    )

def governed_run(sandbox, command: str):
    if _is_destructive_rm(command) or any(re.search(p, command) for p in _DENY_PATTERNS):
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
never contacted for that call. This is a minimal illustration; swap the
regex list for whatever policy engine (OPA, a YAML rules file, an LLM
classifier) fits your risk model — the wrapper shape around
`sandbox.commands.run()` is the actual pattern, not the matching logic.
