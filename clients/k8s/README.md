# Generated Kubernetes Typed Clientset

> [!WARNING]
> **DO NOT EDIT CODE IN THIS DIRECTORY DIRECTLY.**
> The files in this directory are auto-generated from API definitions in `api/` and `extensions/api/`.

## Purpose

This directory provides standard `client-go` typed machinery for custom Kubernetes controllers and operators:
* `clientset/` — Versioned typed clientset for `agents.x-k8s.io` and `extensions.agents.x-k8s.io`.
* `informers/` — Shared informer factories for caching and event handling.
* `listers/` — Typed listers for cache lookups.

If you are writing application code or an agent rather than a Kubernetes controller, use the high-level Go client in [`clients/go/`](../go) instead.

## How to Regenerate

After modifying API structs or kubebuilder markers in `api/` or `extensions/api/`, regenerate these packages:

```bash
make fix-go-generate
```

Direct script invocation:
```bash
dev/tools/client-gen-go.sh
```
