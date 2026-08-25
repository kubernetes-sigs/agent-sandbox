# Deploying sandboxd: two topologies

The SDK talks to sandboxd the same way regardless of how it's deployed — but
**where the sandboxd process runs decides where your commands execute and which
binaries are available.**

## Mental model (read this first)

- A **shared volume** (`/workspace`) shares *files/bytes* between containers.
- It does **not** share *binaries* — each container has its own root filesystem
  (its own overlayfs). `python3`, `npm`, and the dynamic linker come from a
  container's **image**, not from the volume.
- `ProcessService.Execute` runs a command in **whatever container the sandboxd
  process lives in**. So the tools your agent invokes must exist in *that*
  container's image.

That single fact is why there are two topologies.

## The two options

| | **A — runtime image** (default) | **B — inject binary** (no-rebuild) |
|---|---|---|
| File | [`a-runtime-image.yaml`](a-runtime-image.yaml) | [`b-inject-binary.yaml`](b-inject-binary.yaml) |
| Where sandboxd runs | dedicated `sandboxd` container | inside your existing app container |
| Commands execute in | the sandboxd image | your app image |
| Tools (npm, python…) come from | the sandboxd image (bake them in) | your app image (already there) |
| Needs an image rebuild? | yes — `FROM sandboxd` + your tools | **no** |
| Isolation | stronger (locked-down runtime) | weaker (full app image) |
| Use when | you can build/control the runtime image | you must reuse an existing image untouched |

**Recommendation:** use **A** by default — it's the isolation-first model. Reach
for **B** only when you can't rebuild the execution image.

### A — runtime image (default)
sandboxd is a dedicated container and *is* the execution environment. To give the
agent tools, build your own image `FROM` the sandboxd image and add them. Untrusted
code runs in that controlled image, not your application.

### B — inject the binary (no rebuild)
An initContainer copies the sandboxd **binary** into a shared volume, and your
existing, tool-rich app image runs it as its command. Because sandboxd now runs
inside the app image's filesystem, `npm install` finds `npm` there — with **no
change to the app image**. Trade-off: agent code runs in your full app image, and
sandboxd runs as the container's main process (PID 1).

## Try it

Apply **one** of the two (both define a `sandboxd-warmpool` in `default`):

```console
# Option A (default)
$ kubectl apply -f a-runtime-image.yaml

# ...or Option B (no-rebuild)
$ kubectl apply -f b-inject-binary.yaml
```

Then run the SDK example against it — the client code is identical either way:

```console
$ go run ./examples/sandboxd-sandbox/client -warmpool sandboxd-warmpool -namespace default
```

## Verify which topology you have

The litmus test is a tool that exists in the app image but not in the base
sandboxd image — e.g. `npm` with B's `node:22-slim`:

```console
$ go run ./examples/sandboxd-sandbox/client -warmpool sandboxd-warmpool \
    -namespace default -cmd 'npm --version'
```

- Under **B**, this prints the npm version with `exit=0`: the command executed
  inside the app image, whose tools are all available — with no image rebuild.
- Under **A**, it reports `exit=127` with `npm: not found` on stderr: the
  shared `/workspace` volume carries files, not the neighboring image's
  binaries, so only tools baked into the runtime image are reachable.

That pair of results *is* the mental model above, observed live.

See [`../client/README.md`](../client/README.md) for the client walkthrough.
