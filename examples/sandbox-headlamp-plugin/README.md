# Agent Sandbox Headlamp plugin prototype

This is an experimental, read-only Headlamp plugin prototype for the Agent Sandbox resources.

The prototype currently provides:

- a `Sandbox` list and detail view;
- links from a Sandbox to its backing Pod and Service;
- a `SandboxClaim` list and detail view;
- links from a Claim to its assigned Sandbox and WarmPool;
- a `SandboxWarmPool` list and detail view with desired and ready replicas.
- a `SandboxTemplate` list and detail view with policy settings;
- links from WarmPools to their referenced SandboxTemplate.

The plugin does not create, update, delete, suspend, or resume resources. It uses the current
user's Kubernetes permissions through Headlamp and requires the Agent Sandbox CRDs to be
installed in the connected cluster.

The TypeScript resource interfaces in `src/resources.ts` are temporary, hand-written projections
of the CRDs. They describe only fields consumed by this read-only prototype and are not a
replacement for the Go API types or generated CRD schemas. A future production version should
consider generating TypeScript types from the CRD OpenAPI schemas.

## Local development

From this directory:

```bash
npm install
npm run tsc
npm run lint
npm run build
```

To load the plugin during local Headlamp development, run `npm run start` and follow the
Headlamp plugin development workflow for the selected Headlamp installation.

This prototype is intentionally kept under `examples/` while the community evaluates the
information architecture and resource relationships. Its long-term repository and release
location have not been decided yet.
