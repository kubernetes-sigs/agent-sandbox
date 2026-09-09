# Chrome in a Sandbox

## Overview

This example runs Chrome in an isolated environment.

Currently, it uses a Docker-based setup. However, it is intended to align with the **Sandbox CRD** model in `agent-sandbox`, where workloads run inside Kubernetes-managed Sandboxes.

This example is actively maintained and serves as the foundation for end-to-end (e2e) tests.

## Example Sandbox

Below is an example of running Chrome inside a Sandbox resource:

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: chrome-sandbox
spec:
  podTemplate:
    spec:
      containers:
        - name: chrome
          image: chrome-sandbox:local
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 9222
```

## How to Run

Save the manifest above as `chrome-sandbox.yaml`, then apply it:

```bash
kubectl apply -f chrome-sandbox.yaml
```
Port-forward to access Chrome debugging endpoint:

```bash
kubectl port-forward pod/chrome-sandbox 9222:9222
```
---

## Current Setup (Docker-based)

This example can be run locally using Docker for development and debugging purposes. It is already integrated with the `agent-sandbox` framework and used in end-to-end (e2e) tests via the Sandbox CRD.

Currently you can test it out by running `run-test`; it builds a local container image and runs it for ~5 seconds. The container starts a VNC server (display `:1`) and Chromium, and exposes the Chrome DevTools Protocol on port 9222 (readiness is probed via `http://localhost:9222/json/version`).

---

## Usage in e2e Tests

The Chrome sandbox is already used in the project’s end-to-end tests.

- The Sandbox manifest is defined in:
  `test/e2e/chromesandbox_test.go`
- The test creates a `Sandbox` resource running Chrome
- This ensures Chrome runs correctly inside a Sandbox environment

The container image is built locally (there is no published `chrome-sandbox` image):

```bash
docker buildx build --load --tag chrome-sandbox:local .
kind load docker-image chrome-sandbox:local   # when running against a kind cluster
```
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

- Improve readiness and health checks for Chrome startup
- Add support for browser automation frameworks (e.g., Selenium, Playwright)
- Expand test coverage for more interactive/browser-based workloads 