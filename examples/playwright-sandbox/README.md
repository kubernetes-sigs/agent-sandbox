# Playwright in a Sandbox

## Overview

This example runs [Playwright](https://playwright.dev/) with Chromium inside an isolated Sandbox environment to scrape web page titles, body snippets, and capture full-page screenshots.

Currently, it uses a Docker-based setup. However, it is intended to align with the **Sandbox CRD** model in `agent-sandbox`, where workloads run inside Kubernetes-managed Sandboxes.

## Example Sandbox

Below is an example of running Playwright inside a Sandbox resource:

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: playwright-sandbox
spec:
  podTemplate:
    spec:
      containers:
      - name: playwright
        # my playwright sandbox
        image: playwright-sandbox:latest
        env:
        - name: TARGET_URL
          value: "https://example.com"
        command:
        - bash
        - -c
        - |
          python3 - <<'PY'
          # Playwright script to browse, screenshot, and print page content
          PY
          sleep infinity
      restartPolicy: Never
```

## How to Run

### Build the Playwright Sandbox Image

```bash
cd examples/playwright-sandbox
docker build -t playwright-sandbox:latest .
```

This image includes:
- Playwright Python package
- Chromium browser binaries
- Node.js and npm (for Playwright MCP)
- Non-root user (playwright) for security

### Deploy to Kubernetes

```bash
# Define the image reference (replace with your registry)
export IMAGE=registry.example.com/playwright-sandbox:latest

# Build and push the image to your registry
docker build -t ${IMAGE} .
docker push ${IMAGE}

# Update the image in the manifest and apply
cat playwright-sandbox.yaml | envsubst | kubectl apply -f -
```

### Check Sandbox status

```bash
kubectl get sandbox playwright-sandbox
kubectl get pods -l app=playwright-sandbox
POD=$(kubectl get pods -l app=playwright-sandbox -o jsonpath='{.items[0].metadata.name}')
kubectl logs ${POD} -c playwright
```

You should see output similar to:
```text
title: Example Domain
screenshot saved at: /home/playwright/screenshot.png
content snippet: Example Domain This domain is for use in illustrative examples in documents...
```

### Retrieve the Screenshot

The manifest appends `sleep infinity` to the command so the container stays
running after the script finishes. This keeps the Sandbox alive and lets you
use `kubectl cp` / `kubectl exec` to retrieve files from the container's
filesystem.

```bash
# Find the pod name
POD=$(kubectl get pods -l app=playwright-sandbox -o jsonpath='{.items[0].metadata.name}')

# Copy the screenshot to your local machine
kubectl cp ${POD}:/home/playwright/screenshot.png ./screenshot.png
```

---

## Mapping to Sandbox Concepts

| Current Setup        | Sandbox Equivalent              |
|---------------------|--------------------------------|
| Docker container     | Sandbox Pod                    |
| Local Playwright     | Playwright inside Sandbox      |
| Script execution     | Container command              |
| Local file output    | Pod filesystem / kubectl cp    |

---

## References
- [Playwright](https://playwright.dev/)
- [Playwright MCP](https://github.com/microsoft/playwright-mcp)
