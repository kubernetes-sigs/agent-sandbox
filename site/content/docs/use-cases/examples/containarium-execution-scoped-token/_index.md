---
title: "Execution-Scoped Token Example"
linkTitle: "Execution-Scoped Token"
weight: 2
description: >
  Demonstrates execution-scoped credentials in a reused Sandbox: a run-bound token delivered only through sandboxd's ProcessConfig.env_vars, egress pinned to a credential proxy with Cilium, and the token revoked when the process exits rather than left to expire.
---
{{% include-file file="additional/examples/containarium-execution-scoped-token/README.md" %}}
