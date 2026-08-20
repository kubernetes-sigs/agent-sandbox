---
applyTo: "api/**/*.go,extensions/api/**/*.go"
---

# Kubernetes API Conventions

- **CRDs as First-Class APIs**: Follow Kubernetes API conventions and kubebuilder markers for CRD types.
- **Label Value Limits**: Do NOT use full resource names as label values (must stay within Kubernetes 63-character limit; hash/truncate if needed).
- **Preview Features**: Do NOT use annotations for alpha/preview features; use new API fields instead.
- **State Tracking**: Prefer `conditions` over a `phase` enum for tracking status.
- **Zero vs. Unset**: Use pointers for optional fields where distinguishing between zero values and unset is required.
- **Avoid Booleans**: Avoid booleans for fields that might evolve to support additional states in the future; prefer string enums.
- **Schema Minimization**: Do NOT expand CRD schemas with redundant fields or overlapping toggles. Maintain declarative CR precedence over global CLI flags.
- **Generated Code**: Never hand-edit `zz_generated.*.go` or files in `k8s/crds/`; regenerate via `make fix-go-generate`.
