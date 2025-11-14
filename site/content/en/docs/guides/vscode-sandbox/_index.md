---
title: "Create a Sandbox with VSCode and Gemini CLI"
linkTitle: "Create a Sandbox with VSCode and Gemini CLI"
weight: 2
description: >
  This guide documents the complete setup and deployment of VSCode and Gemini CLI that runs locally on Kubernetes using Kind (Kubernetes in Docker) with the agent-sandbox controller.
---

## Create a Sandbox with VSCode and Gemini CLI

Navigate to the folder with resources by running:
```bash
cd examples/vscode-sandbox
```

Apply the sandbox manifest with PVC

```bash
kubectl apply -f vscode-sandbox.yaml
```

They can then check the status of the applied resource.
Verify sandbox and pod are running:

```bash
kubectl get sandbox
kubectl get pod sandbox-example

kubectl wait --for=condition=Ready sandbox sandbox-example
```

## Accesing vscode

Port forward the vscode server port.

```
 kubectl port-forward --address 0.0.0.0 pod/sandbox-example 13337
```

Connect to the vscode-server on a browser via  http://localhost:13337 or <machine-dns>:13337

If should ask for a password.

#### Getting vscode password

In a separate terminal connect to the pod and get the password.

```
kubectl exec  sandbox-example --  cat /root/.config/code-server/config.yaml
```

Use the password and connect to vscode.

## Use gemini-cli

Gemini cli is preinstalled. Open a teminal in vscode and use Gemini cli.
