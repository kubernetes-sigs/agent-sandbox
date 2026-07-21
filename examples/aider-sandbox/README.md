# Aider in Agent Sandbox

This guide explains how to deploy [Aider](https://aider.chat/), an AI pair programming tool, inside an isolated Kubernetes environment using Agent Sandbox. By packaging Aider's browser UI into a container and leveraging Agent Sandbox's lifecycle management, you get a disposable, session-scoped workspace with a persistent volume to safely run AI-driven code edits and local shell commands.

## Set up

Clone the repository and switch to this example directory:

```bash
git clone https://github.com/kubernetes-sigs/agent-sandbox.git
cd agent-sandbox/examples/aider-sandbox
```

Create a local Kubernetes environment to host the Agent Sandbox controller and our resources.

```bash
kind create cluster --name agent-sandbox
```

Install the Agent Sandbox controller (core + extensions) by following the [installation guide](https://github.com/kubernetes-sigs/agent-sandbox#installation), then verify it is running:

```bash
kubectl wait --for=condition=Ready pod -l app=agent-sandbox-controller -n agent-sandbox-system --timeout=120s
```

## Deploy Aider

Build the custom Docker image containing Aider and its browser dependencies. Since we are using a local kind cluster, we need to load the image directly into the cluster's nodes so Kubernetes doesn't try to pull it from a remote registry.

> **Note for live clusters:** Push the `aider-sandbox:v1` image to your registry and update `imagePullPolicy: IfNotPresent` in `template.yaml`.

```bash
docker build -t aider-sandbox:v1 .
kind load docker-image aider-sandbox:v1 --name agent-sandbox
```

Once the image is loaded, securely provide your LLM API key to the cluster. Run the following commands to create a Kubernetes secret.

```bash
export OPENAI_API_KEY=<OPENAI_API_KEY>
export NAMESPACE=default

kubectl create secret generic llm-secrets --from-literal=openai-api-key=${OPENAI_API_KEY} --namespace=${NAMESPACE}
```

With the prerequisites in place, apply the Agent Sandbox Custom Resource manifests. This will deploy the Sandbox template, initialize the warm pool for instant access, and bind a user claim to provision the workspace and clone your target repository.

> **Note:** Before applying, edit template.yaml and change `REPO_URL` to point to your repository.

```bash
kubectl apply -f template.yaml
kubectl apply -f warmpool.yaml
kubectl apply -f claim.yaml
```

## Test the environment

To access the Aider browser interface, we need to route traffic from your local machine to the isolated pod. The following commands dynamically retrieve the name of your provisioned sandbox pod and set up a port-forward to Aider's Streamlit server. Once executed, open `http://localhost:8501` in your web browser.

```bash
sandbox_name=$(kubectl get sandboxclaim user-session-aider -o jsonpath='{.status.sandbox.name}')
pod_name=$(kubectl get sandbox "${sandbox_name}" -o jsonpath='{.metadata.annotations.agents\.x-k8s\.io/pod-name}')
kubectl port-forward "pod/${pod_name}" 8501:8501
```

Because the sandbox automatically cloned the target GitHub repository into the `/workspace` directory, Aider is fully context-aware from the moment you connect. You can immediately ask it to analyze the codebase, write new features, or run local shell commands. Here is an example of what that looks like:

```txt
Q: Can you please say what is this project about?

A: Based on the file summaries you provided, this project appears to be related to managing Kubernetes resources, specifically focusing on sandbox environments. It includes various components for handling sandbox templates, claims, warm pools, and lifecycle management. The project seems to involve both Go and Python codebases, with functionalities for Kubernetes client interactions, sandbox management, and possibly some form of resource orchestration or automation within Kubernetes clusters. The presence of files related to OpenTelemetry suggests that there might be tracing or monitoring capabilities integrated into the project as well.
```

## Clean up

When you are finished experimenting, you can easily tear down the entire local environment. Deleting the kind cluster will cleanly remove all associated sandboxes, persistent volumes, and secrets.

```bash
kind delete cluster --name agent-sandbox
```
