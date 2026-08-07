# Generated protobuf/gRPC stubs

This package holds the Python ProcessService stubs generated from
`packages/sandboxd/spec/process/v1/process.proto`, used by the sandboxd
runtime command executor.

**Do not edit by hand.** Regenerate from the repo root with:

```bash
cd packages/sandboxd/spec
buf generate --template buf.gen.python.yaml
```

This produces `process/v1/process_pb2.py`, `process_pb2.pyi`, and
`process_pb2_grpc.py` under this directory (alongside the committed
`__init__.py` package markers).

The generated `process_pb2_grpc.py` imports its sibling with an absolute
path rooted at the proto package (`from process.v1 import process_pb2`).
`k8s_agent_sandbox/commands/_process_stubs.py` puts this `_proto` directory
on `sys.path` so that import resolves; import the stubs via that shim
(`from k8s_agent_sandbox.commands._process_stubs import process_pb2,
process_pb2_grpc`) rather than by a `k8s_agent_sandbox._proto...` path.
