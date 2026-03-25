# Chrome in a Sandbox

## Overview

This example runs Chrome in an isolated environment.

Currently, it uses a Docker-based setup. However, it is intended to align with the **Sandbox CRD** model in agent-sandbox, where workloads run inside Kubernetes-managed Sandboxes.

This example is also used in end-to-end (e2e) tests and is not obsolete.

---

## Current Setup (Docker-based)

This example runs Chrome in a container; we are starting by running it in a Docker container, but the plan is to run it in a Sandbox as we stand up the infrastructure there.

Currently you can test it out by running `run-test`; it will build a (local) container image, then run it. The image will capture screenshots roughly every 100ms so you can observe the progress as Chrome launches and opens (currently) https://google.com

The screenshots are in an unusual xwg format, so the script depends on the `convert`
utility to convert those to an animated gif.

---

## Usage in e2e Tests

The Chrome sandbox is already used in the project’s end-to-end tests.

- The Sandbox manifest is defined in:
  `test/e2e/chromesandbox_test.go`
- The test creates a `Sandbox` resource running Chrome
- This ensures Chrome runs correctly inside a Sandbox environment

The container image used is available at:

`registry.k8s.io/chrome-sandbox`

---

## Using this example with Sandbox CRD

In a Sandbox-based setup:

- The Chrome container runs inside a `Sandbox` resource  
- The Sandbox controller manages lifecycle and isolation  
- Chrome can be accessed via a debugging endpoint (e.g., port `9222`)  
- Users interact with it using port-forwarding or services  

---

## Mapping to Sandbox Concepts

| Current Setup        | Sandbox Equivalent              |
|---------------------|--------------------------------|
| Docker container     | Sandbox Pod                    |
| run-test script      | Sandbox lifecycle              |
| Local Chrome         | Chrome inside Sandbox          |
| Port exposure        | Kubernetes port-forward/service|

---

## Plans / Future Improvements

- Move to Sandbox  
- Implement a better test for readiness  
- Maybe support selenium / playwright to make this a more useful example  
- Incorporate into our e2e tests  