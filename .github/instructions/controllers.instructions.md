---
applyTo: "controllers/**/*.go,extensions/controllers/**/*.go,internal/**/*.go"
---

# Controller Guidelines

- **Controller-Runtime Best Practices**: Ensure Reconcile loops are idempotent and safe under retries. Respect `context.Context` cancellation and avoid goroutines without lifetime ownership.
- **Spec Immutability**: The `spec` of the primary Custom Resource (CR) being reconciled is user-owned; never modify and save it back to the API server in the reconciler. Controllers may only update `status` of the primary CR or `spec` of secondary/target objects.
- **Label Value Limits**: Do NOT use full resource names directly in label values (must stay within Kubernetes 63-character limit; hash/truncate in controller logic if needed).
- **Error Wrapping**: Always wrap errors with context (`fmt.Errorf("...: %w", err)`). Surface meaningful conditions on the resource status rather than swallowing errors.
- **Logging Discipline**: Use `logr.Logger` (`log.FromContext(ctx)`) with structured key/value pairs (never `fmt.Sprintf`). Reserve `logger.Info` / `V(0)` for major state transitions (e.g., resource created, claim adopted) and require `V(4)` for routine steady-state checks or cache lookups.
- **Metrics Cardinality**: Never introduce high or unbounded cardinality Prometheus labels (e.g., pod names, UIDs, timestamps, raw errors). Map dynamic values to a small, fixed enum via normalization.
- **Call-Site Impact**: When modifying helper predicates or error returns, audit all call sites across reconcilers to prevent unintended side effects (e.g., premature cache drops or queue evictions).
