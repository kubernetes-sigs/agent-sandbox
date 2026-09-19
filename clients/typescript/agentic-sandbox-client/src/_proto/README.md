# Generated protobuf/Connect-ES stubs

This directory holds the TypeScript `ProcessService` stubs generated from `packages/sandboxd/spec/process/v1/process.proto`, for use by the sandboxd runtime connectivity layer.

**Do not edit by hand.** Regenerate from the repo root with:

```bash
cd packages/sandboxd/spec
buf generate --template buf.gen.typescript.yaml
```

This produces `process/v1/process_pb.ts` under this directory. 
It is generated with [protoc-gen-es](https://github.com/bufbuild/protobuf-es) v2 (`target=ts`) and depends on `@bufbuild/protobuf`, which the SDK declares only as an optional peer dependency — this directory is excluded from the default `tsc` build (see `tsconfig.json`'s `exclude`) so that unused proto code does not ship in `dist/` for consumers who never touch the sandboxd runtime layer, and from biome (see `biome.json`'s `files.includes`).
