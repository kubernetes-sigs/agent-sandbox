# Generated protobuf/gRPC stubs

This package holds the Python ProcessService and FileWatcherService stubs
generated from `packages/sandboxd/spec/process/v1/process.proto` and
`packages/sandboxd/spec/watcher/v1/watcher.proto`, used by the sandboxd
runtime command executor and file watcher.

**Do not edit by hand.** Regenerate from the repo root with:

```bash
cd packages/sandboxd/spec
buf generate --template buf.gen.python.yaml
```

This produces `process/v1/process_pb2.py`, `process_pb2.pyi`, and
`process_pb2_grpc.py` alongside `watcher/v1/watcher_pb2.py`,
`watcher_pb2.pyi`, and `watcher_pb2_grpc.py` under this directory (alongside
the committed `__init__.py` package markers).

The generated `*_pb2_grpc.py` modules import their siblings with absolute
paths rooted at the proto package (`from process.v1 import process_pb2`,
`from watcher.v1 import watcher_pb2`). `k8s_agent_sandbox/commands/_process_stubs.py`
and `k8s_agent_sandbox/watcher/_watcher_stubs.py` put this `_proto` directory
on `sys.path` so that imports resolve; import the stubs via those shims
rather than by a `k8s_agent_sandbox._proto...` path.
