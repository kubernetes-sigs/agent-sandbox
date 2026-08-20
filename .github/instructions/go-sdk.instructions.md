---
applyTo: "clients/go/**/*"
---

# Go SDK Guidelines

- **Idiomatic Go**: Focus on clean Go client patterns wrapping `SandboxClaim` lifecycle and connectivity (port-forward, gateway, direct).
- **API Stability**: Ensure error handling is robust, methods are backward-compatible, and examples are clear.
- **Generated vs Hand-Written**: Note that `clients/go/` is hand-written, while `clients/k8s/` is generated (do not hand-edit `clients/k8s/`).
