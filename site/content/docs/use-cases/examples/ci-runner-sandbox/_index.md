---
title: "CI Runner Sandbox"
linkTitle: "CI Runner Sandbox"
weight: 2
description: >
  Run each CI job in its own pre-warmed Agent Sandbox: a Python driver claims a sandbox per job, runs build/test commands, echoes logs back to the CI system, and destroys the sandbox even if the job crashes.
---
{{% include-file file="additional/examples/ci-runner-sandbox/README.md" %}}
