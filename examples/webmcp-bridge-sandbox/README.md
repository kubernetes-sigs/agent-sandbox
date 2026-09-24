# WebMCP Bridge Sandbox

## Overview

[WebMCP](https://github.com/webmachinelearning/webmcp) is a browser-side
proposal: instead of an agent scraping a page's DOM or driving it blindly
through clicks and screenshots, the page itself registers tools — a name, a
description, a JSON schema, and a handler — on `document.modelContext`. An
agent discovers what's available via `getTools()` and invokes one via
`executeTool()`, the same shape as any other MCP tool list.

The catch: those tools only exist inside a live browser that has actually
loaded the page. A backend service has no DOM, so it structurally can't host
`document.modelContext` — something has to drive a real browser. This
example runs that "something" — a small bridge built on
[Playwright](https://playwright.dev/) — inside a Sandbox pod, and re-exposes
whatever tools the page registered as two MCP tools:
`list_browser_tools()` and `call_browser_tool(tool_name, arguments)`.

This uses the third-party [`fastmcp`](https://gofastmcp.com) PyPI package,
not the official `mcp.server.fastmcp` that `examples/mcp-server-sandbox`
uses — not a typo. `bridge.py` needs to start Playwright and the MCP
server in the same asyncio event loop (see the module docstring for why),
and `fastmcp`'s `run_async()` is what makes that possible.

For a self-contained demo with nothing to configure, the same container also
serves a small static `demo-page/index.html` that registers two tools of its
own — `get-server-time` (read-only) and `increment-counter` (mutates
in-page state) — so the example works out of the box against itself. Point
`TARGET_PAGE_URL` at any other WebMCP-enabled page to bridge that instead.

Startup waits up to 5 seconds for the target page to have registered at
least one tool before handing off to the MCP server, since
`document.modelContext` existing only means the API installed, not that
registration finished — a page that registers tools asynchronously (after
its own fetch, or a framework mount) could otherwise race with the bridge's
very first call. If nothing has registered within that window, the bridge
proceeds anyway and logs a warning — `list_browser_tools` re-checks the
page fresh on every call, so a page that registers its tools late is still
picked up on a later call, it just isn't guaranteed to be ready on the
first one.

No browser ships `document.modelContext` yet, and the WebMCP repository
publishes no reference polyfill (it's explainer/discussion only at this
stage) — `demo-page/index.html` installs a hand-rolled stub only when the
real API is absent, matching just enough of the surface (register/get/
execute) to be useful, not a spec-conformant implementation.

## Example Sandbox

Also committed as [`webmcp-bridge-sandbox.yaml`](./webmcp-bridge-sandbox.yaml)
in this directory, so `kubectl apply` below has something real to point at:

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: webmcp-bridge-sandbox
spec:
  podTemplate:
    spec:
      # No securityContext hardening (runAsNonRoot, etc.) here, unlike
      # examples/mcp-server-sandbox — Chromium's own sandboxing wants
      # more privilege than a locked-down pod securityContext allows by
      # default (it needs to create its own user namespaces / apply
      # seccomp itself). Running as non-root is still possible but needs
      # Chromium launched with --no-sandbox or an image built for it;
      # left out here to keep the example minimal.
      containers:
        - name: webmcp-bridge
          image: webmcp-bridge-sandbox:local
          imagePullPolicy: IfNotPresent
          env:
            - name: TARGET_PAGE_URL
              value: "http://localhost:8090/"  # the bundled demo page
          # No resources block here to keep the example minimal, but a
          # real deployment should set one — headless Chromium's own
          # footprint alone is comfortably in the hundreds-of-MB range
          # (examples/gke-swap sizes Chrome pods around 2Gi as a working
          # limit), before counting the target page's own memory use.
      restartPolicy: Never
```

## How to Run

### Try it locally first, no cluster needed

```bash
cd examples/webmcp-bridge-sandbox
docker build -t webmcp-bridge-sandbox:local .

# Keep the container alive so you can exec into it and drive the bridge
# directly (the real CMD is a stdio MCP server, which exits immediately
# with no client attached — fine once you're pointing a real MCP client
# at it via kubectl exec, awkward for a one-off local check).
docker run -d --name webmcp-bridge-demo --entrypoint sh \
  webmcp-bridge-sandbox:local \
  -c "python3 -m http.server 8090 --directory demo-page & sleep infinity"

docker exec webmcp-bridge-demo python3 -c "
import asyncio, sys
sys.path.insert(0, '/app')
import bridge

async def main():
    await bridge._start_bridge()
    # .fn intentionally bypasses FastMCP's @mcp.tool() wrapper to call the
    # underlying function directly, for a one-off local check with no MCP
    # client involved — it's FastMCP's own documented way to get at the
    # unwrapped function, not a private/internal implementation detail.
    print(await bridge.list_browser_tools.fn())
    print(await bridge.call_browser_tool.fn(tool_name='get-server-time', arguments={}))
    print(await bridge.call_browser_tool.fn(tool_name='increment-counter', arguments={'by': 3}))

asyncio.run(main())
"

docker rm -f webmcp-bridge-demo
```

You should see the two registered tools listed, a real ISO timestamp from
the page, and the counter incrementing by 3.

### Running the tests

`test_bridge.py` exercises `WebMCPBridge` against a fake `Page` object, not
a real browser — no Docker build, Playwright install, or cluster needed:

```bash
pip install pytest fastmcp playwright
python3 -m pytest test_bridge.py -v
```

`dev/ci/presubmits/test-webmcp-bridge-sandbox` runs the same thing, matching
`examples/mcp-server-sandbox`'s presubmit pattern.

### On a cluster

```bash
# Build and push (or load into a local registry/kind cluster) as usual for
# this repo's examples:
docker build -t <your-registry>/webmcp-bridge-sandbox:latest examples/webmcp-bridge-sandbox
docker push <your-registry>/webmcp-bridge-sandbox:latest

# Edit webmcp-bridge-sandbox.yaml's image field to your pushed tag, then apply it:
kubectl apply -f examples/webmcp-bridge-sandbox/webmcp-bridge-sandbox.yaml

# The real CMD is a stdio MCP server, so drive it the same way
# examples/mcp-server-sandbox does — kubectl exec as the stdio transport:
kubectl exec -i webmcp-bridge-sandbox -- python3 bridge.py
```

Each `kubectl exec` above starts a *second*, independent `bridge.py`
process with its own Playwright/Chromium instance — it doesn't attach to
whatever the pod's own `CMD` is already running. Matches
`examples/mcp-server-sandbox`'s convention, but worth knowing: two
lingering Chromium instances in the same pod means double the memory
footprint from the note above, not one.

## Governing tool calls

This example calls `list_browser_tools`/`call_browser_tool` with no policy
check in front of them — deliberately, to keep the example minimal and free
of a third-party dependency. A real deployment almost certainly wants to
gate `call_browser_tool` the same way you'd gate any other agent-initiated
side effect: classify `tool_name` (at minimum, read vs. mutating — the
policy engine's own condition logic generally can't introspect a page's tool
metadata, so this classification usually has to happen in your own code
before the call) and deny or require approval for the mutating ones before
they ever reach the page. `demo-page/index.html`'s `increment-counter` tool
exists specifically to give you something mutating to test that
classification against.

`call_browser_tool(tool_name: str, arguments: dict)` is intentionally
generic — a single MCP tool that dispatches to whichever WebMCP tool the
page happens to have registered, discovered at runtime via
`list_browser_tools`. That means an MCP client gets no per-tool schema
enforcement from the bridge itself (unlike a dedicated MCP tool with its
own typed signature): `arguments` passes through to
`document.modelContext.executeTool()` as-is, and validating it against the
tool's own `inputSchema` is the page's responsibility, not the bridge's.
That's a reasonable tradeoff for a bridge that has to stay generic across
arbitrary pages, but worth calling out explicitly if you're used to MCP
tools that validate their own arguments.

## Networking notes if you point this at a page on your host machine

Two gotchas surfaced repeatedly enough during development to be worth
calling out explicitly:

- Sandbox pods block RFC1918 (`10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`) egress by default via a per-`SandboxTemplate`
  `NetworkPolicy`. Reaching a dev server on your own machine needs a
  supplemental `NetworkPolicy` on top of that default — same idea as the
  per-Sandbox egress policy in
  [`examples/containarium-execution-scoped-token/networkpolicy.yaml`](../containarium-execution-scoped-token/networkpolicy.yaml),
  just with an `ipBlock` for an external host address instead of a
  `podSelector` for an in-cluster destination:

  ```yaml
  apiVersion: networking.k8s.io/v1
  kind: NetworkPolicy
  metadata:
    name: allow-dev-server-egress
  spec:
    podSelector: {}
    policyTypes: [Egress]
    egress:
      - to:
          - ipBlock:
              cidr: <your-host-ip>/32
        ports:
          - protocol: TCP
            port: 5173
  ```
- If you're on Docker Desktop, the "reach the host from a container"
  gateway address (`192.168.65.254` on macOS) can collide with your
  cluster's own pod CIDR if that's also `192.168.0.0/16` — the packet gets
  routed into the pod overlay instead of out to the host, and the symptom
  is a timeout, not a refusal. Your host's real LAN IP (shown by most dev
  servers' own "Network:" startup line) is a more reliable target.

## Mapping to Sandbox Concepts

| Piece                       | Sandbox Equivalent                          |
| ---------------------------- | -------------------------------------------- |
| `bridge.py`                  | The Sandbox pod's main process (MCP server)  |
| Headless Chromium (Playwright) | Runs inside the Sandbox pod's container    |
| `demo-page/index.html`        | Served from the same container for a self-contained demo; point `TARGET_PAGE_URL` elsewhere for a real page |
| `list_browser_tools`/`call_browser_tool` | The MCP surface an external client talks to, same shape as [`examples/mcp-server-sandbox`](../mcp-server-sandbox) |

## References

- [WebMCP](https://github.com/webmachinelearning/webmcp)
- [Playwright](https://playwright.dev/)
- [examples/mcp-server-sandbox](../mcp-server-sandbox) — the stdio-transport-over-`kubectl exec` pattern this example reuses
- [examples/playwright-sandbox](../playwright-sandbox) — running Playwright in a Sandbox for browser automation without WebMCP
