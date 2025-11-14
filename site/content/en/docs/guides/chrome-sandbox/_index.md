---
title: "Chrome in a Sandbox"
linkTitle: "Chrome in a Sandbox"
weight: 2
description: >
  This guide documents the complete setup and deployment of Chrome in a Sandbox that runs locally on Kubernetes using Kind (Kubernetes in Docker) with the agent-sandbox controller.
---
# chrome in a Sandbox

Navigate to the folder with resources by running:
```bash
cd examples/chrome-sandbox
```

This example runs Chrome in a container; we are starting by running it in a Docker container,
but the plan is to run it in a Sandbox as we stand up the infrastructure there.

Currently you can test it out by running `run-test`; it will build a (local) container image,
then run it.  The image will capture screenshots roughly every 100ms so you can observe
the progress as Chrome launches and opens (currently) https://google.com

The screenshots are in an unusual xwg format, so the script depends on the `convert`
utility to convert those to an animated gif.

Plans:

* Move to Sandbox
* Implement a better test for readiness
* Maybe support selenium / playwright to make this a more useful example
* Incorporate into our e2e tests
