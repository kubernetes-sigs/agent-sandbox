---
title: "CI/CD"
linkTitle: "CI/CD"
weight: 4
description: >
  Infrastructure patterns for using Agent Sandbox in CI/CD pipelines — network control, disruption protection, and programmatic lifecycle management.
---

## Overview

Agent Sandbox can be integrated into CI/CD pipelines to provide isolated environments for running tests, validating generated code, or executing untrusted build steps. The sandbox's Kubernetes-native API and [Python client](/docs/python-client/) enable programmatic sandbox lifecycle management from CI scripts.

While Agent Sandbox does not include dedicated CI/CD examples yet, the infrastructure guides below are directly relevant to pipeline scenarios — controlling network access for sandboxes and protecting running sandboxes from disruptions during cluster maintenance.

## Why Use a Sandbox in CI/CD?

- **Isolated execution** — Each pipeline run can create its own sandbox, preventing test pollution and side effects between runs.
- **Network control** — Use Kubernetes Network Policies to restrict what sandboxes can access, preventing untrusted code from reaching internal services.
- **Disruption protection** — Protect sandbox pods from voluntary evictions (node drains, cluster upgrades) using PodDisruptionBudgets.
- **Programmatic control** — The [Python client](/docs/python-client/) (`agentic-sandbox-client`) provides a high-level interface for creating, managing, and destroying sandboxes from scripts.

## Related Infrastructure Guides

- [Composing Sandbox with Network Policies](/docs/use-cases/examples/network-policies/) — Shows how to compose `Sandbox` with `NetworkPolicy`, `Ingress`, and `Service` resources using [KRO (Kubernetes Resource Orchestrator)](https://kro.run/docs/overview). Defines a higher-level `AgenticSandbox` CRD via a `ResourceGraphDefinition` that bundles these resources together. Also covers custom controller and Helm approaches.
- [Manual PodDisruptionBudget Configuration](/docs/use-cases/examples/manual-pdb/) — Demonstrates a shared PDB per namespace approach using `maxUnavailable: 0` to protect all labeled sandbox pods from voluntary eviction. Sandboxes opt in via the `sandbox-disruption-policy: "manual-protection"` label. Note: manual PDBs do not clean themselves up — coordinate with your team before deletion.
- [Python Client](/docs/python-client/) — The `agentic-sandbox-client` SDK for programmatic sandbox lifecycle management via the Kubernetes API.
