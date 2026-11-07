# JupyterLab under a browser path prefix

This example serves a JupyterLab sandbox through the sandbox-router at a
browser-safe path:

```text
/sandboxes/<namespace>/<sandbox-id>/<port>/...
```

It shows the contract that
[#1109](https://github.com/kubernetes-sigs/agent-sandbox/issues/1109) asks for:
the sandbox identity stays in the browser-visible path, and nothing in the
browser needs a wildcard host or custom headers.

## How it works

The router is started with two flags:

- `--path-routing-prefix=/sandboxes` lets a request address a sandbox by path.
- `--path-routing-preserve-prefix` forwards the full path to the sandbox
  instead of stripping the prefix.

JupyterLab is started with `--ServerApp.base_url` set to the same prefix
(`sandbox.yaml`). It then emits prefixed redirects, `Set-Cookie` paths, asset
URLs, kernelspec URLs and kernel WebSockets on its own, so the router does not
rewrite response bodies.

Without `--path-routing-preserve-prefix`, the router strips the prefix and
rewrites upstream `Location` and `Set-Cookie` paths under it. That mode is for
apps that have no base-path setting.

## Run it

Requires a cluster with the agent-sandbox controller and the sandbox-router
built from this repo (`sandbox-router/`).

The router must run with the path-routing flags. Add these to the container
`args` in `sandbox-router/deploy/deployment.yaml` (the manifest there ships
without them, so header-only behavior is unchanged for everyone else):

```yaml
        args:
        # ...existing args...
        - --path-routing-prefix=/sandboxes
        - --path-routing-preserve-prefix
```

Then:

```bash
kubectl apply -f examples/jupyterlab-base-path/sandbox.yaml
kubectl wait --for=condition=Ready sandbox/jupyterlab-base-path --timeout=300s

# The router Service is sandbox-router-svc (sandbox-router/deploy/service.yaml).
kubectl port-forward svc/sandbox-router-svc 8080:8080 &

# Run the same contract checks against the router.
BASE_URL=http://127.0.0.1:8080 \
PREFIX=/sandboxes/default/jupyterlab-base-path/8888 \
  python3 examples/jupyterlab-base-path/verify.py
```

`verify.py` checks that:

- the app is not reachable without the prefix,
- a prefix without a trailing slash redirects to a prefixed `Location`,
- the lab page and its assets are under the prefix,
- kernelspec resource URLs are under the prefix,
- cookies are scoped to the prefix,
- a kernel starts over REST and executes code over a WebSocket under the prefix.

## Notes

- Authentication is disabled (`--ServerApp.token=''`) to keep the demo short.
  Do not expose this sandbox outside the cluster as written.
- Keep `--path-routing-preserve-prefix` on for this example. In the default
  mode the router strips the prefix, so JupyterLab receives paths that do not
  match its `base_url` and requests fail.
- The XSRF check stays on. With `base_url` set, the `_xsrf` cookie is scoped to
  the sandbox prefix instead of `Path=/`, so sandboxes do not collide.
- `base_url` hardcodes the namespace and sandbox name. If the sandbox is adopted
  from a warm pool, its pod name can differ from its sandbox name, so derive the
  `base_url` from the name the router routes to.
