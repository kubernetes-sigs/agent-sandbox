# Hello World on Kubernetes Sandbox

This document describes how to build a simple "Hello World" Docker image, push it to Google Artifact Registry, and deploy it to a Kubernetes cluster using a custom `Sandbox` resource.

## Prerequisites

1.  **Docker:** Docker installed and running on your local machine. See `go/docker`.
2.  **gcloud CLI:** Google Cloud SDK installed and configured. See `go/gcloud-cli`.
3.  **kubectl:** Kubernetes command-line tool installed. See `go/kubectl`.
4.  **Google Cloud Project:** A GCP project with Artifact Registry API enabled.
5.  **Artifact Registry Repository:** A Docker repository created in Artifact Registry.
6.  **Kubernetes Cluster:** Access to a Kubernetes cluster where you have permissions to deploy resources.

## Configuration

Please replace the placeholder values below with your actual environment details:

*   **USERNAME:** `shrutinair` (Example)
*   **LOCATION:** `us-central1` (e.g., your Artifact Registry region)
*   **PROJECT:** `shruti-test-agent-sandbox` (Your GCP Project ID)
*   **REPOSITORY:** `shrutinair-repository` (Your Artifact Registry repository name)
*   **IMAGE_NAME:** `hello-world`
*   **IMAGE_TAG:** `latest`

## Files

*   `Dockerfile`: Defines the instructions to build the Docker image.
*   `hello-world.yaml`: Kubernetes manifest for the `Sandbox` custom resource.

## Steps

**1. Build and Run the Docker Image**
Open a terminal in the directory containing the Dockerfile.

```bash
# Build the image:
# Example: 
#docker build -t shrutinair-hello-world .
docker build -t {USERNAME}-{IMAGE_NAME} .

# Run the image:
# Example:
#docker run --rm shrutinair-hello-world
docker run --rm {USERNAME}-{IMAGE_NAME}
```

**2. Configure Docker Authentication for Artifact Registry**

```bash
# Example:
# gcloud auth configure-docker us-central1-docker.pkg.dev
gcloud auth configure-docker {LOCATION}-docker.pkg.dev
```

**3. Tag and Push the Image to Artifact Registry**  

```bash
# Tag the image with a version.
# Example: 
# docker tag shrutinair-hello-world us-central1-docker.pkg.dev/shruti-test-agent-sandbox/shrutinair-repository/hello-world:latest
docker tag {USERNAME}-{IMAGE_NAME} {LOCATION}-docker.pkg.dev/{PROJECT}/{REPOSITORY}/{IMAGE_NAME}:{IMAGE_TAG}

# Push the image to Artifact Registry.
# Example:
# docker push us-central1-docker.pkg.dev/shruti-test-agent-sandbox/shrutinair-repository/hello-world:v1
docker push {LOCATION}-docker.pkg.dev/{PROJECT}/{REPOSITORY}/{IMAGE_NAME}:{IMAGE_TAG}
```
**4. Update the image path in `hello-world.yaml` with your configuration**

```yaml
apiVersion: agents.x-k8s.io/v1alpha1
kind: Sandbox
metadata:
  name: shrutinair-hello-world # Replaced {USERNAME}
spec:
  podTemplate:
    spec:
      containers:
      - name: my-container
        # Replace placeholders with your Artifact Registry details
        image: us-central1-docker.pkg.dev/shruti-test-agent-sandbox/shrutinair-repository/hello-world:latest
      restartPolicy: Never
```

**5. Deploy to Kubernetes**
Ensure your kubectl context is pointing to the correct cluster. Apply the manifest:

```bash
kubectl apply -f hello-world.yaml
```
This will create a Sandbox resource named `{USERNAME}-hello-world`. The Sandbox controller will then provision the underlying Pod.

**6. Check Sandbox status**

```bash
# 
kubectl get sandbox shrutinair-hello-world

# Find the Pod: The Sandbox controller likely creates a Pod. 
# The naming convention depends on the controller's implementation. Look for pods related to your sandbox:
kubectl get pods

# Look for a pod name possibly prefixed with shrutinair-hello-world.
# Check Pod Status: Once you identify the pod name (e.g., shrutinair-hello-world-xxxxx):
kubectl describe pod <POD_NAME>

```

**6. Verify Container Logs**

```
# Get Logs: While the pod is running or after it has completed:
kubectl logs <POD_NAME> -c my-container
```

You should see the output: `Hello, World from Kubernetes!`

