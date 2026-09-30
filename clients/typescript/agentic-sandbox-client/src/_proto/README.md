# Generated protobuf/Connect-ES stubs

This directory holds the TypeScript `ProcessService` stubs generated from `packages/sandboxd/spec/process/v1/process.proto`, for use by the sandboxd runtime connectivity layer.

**Do not edit by hand.** Regenerate from the repo root with:

```bash
cd packages/sandboxd/spec
buf generate --template buf.gen.typescript.yaml
```

This produces `process/v1/process_pb.ts` under this directory. 
It is generated with [protoc-gen-es](https://github.com/bufbuild/protobuf-es) v2 (`target=ts`) and depends on `@bufbuild/protobuf`, which the SDK declares only as an optional peer dependency. `@bufbuild/protobuf` (and `@connectrpc/connect` / `@connectrpc/connect-node`) are also declared as devDependencies so this directory builds along with the rest of `src/` and ships as `dist/_proto/process/v1/process_pb.js` — `process.ts` loads it via a lazy dynamic `import()` at runtime, only when `sandbox.commands.run()` is first called, so consumers who never touch the sandboxd runtime layer never need the optional peers installed. This directory is excluded from biome (see `biome.json`'s `files.includes`) since it is generated code, and its `.d.ts` is never referenced from the SDK's public exports (`index.ts`), so a files-only consumer's `tsc`/type-checking never resolves the optional peer types either.
