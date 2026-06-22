# Reference: original rl-tunix template

`sandbox_template.yaml.j2` is the **original** Jinja2 SandboxTemplate from the
R2E-Gym `agentic-sandbox-integration` branch
(`src/r2egym/agenthub/runtime/templates/sandbox_template.yaml.j2`), included
verbatim for comparison. It is **not** applied by this example.

The manifests this example actually uses (in the parent `manifests/` directory)
modernize it for current Agent Sandbox:

| | Original `.j2` (reference) | This example |
| :--- | :--- | :--- |
| API version | `extensions.agents.x-k8s.io/v1alpha1` | `extensions.agents.x-k8s.io/v1beta1` |
| Isolation | `runtimeClassName: gvisor` (required) | unset by default; opt in with `RUNTIME_CLASS=gvisor` |
| Node pinning | `nodeSelector` required | optional |
| Rendering | rendered per-image by Python (Jinja2) | static YAML, or built as dicts by `warmpool.py` |

The corresponding WarmPool field rename (`size`/`templateRef` →
`replicas`/`sandboxTemplateRef`) is described in the top-level `README.md`.
