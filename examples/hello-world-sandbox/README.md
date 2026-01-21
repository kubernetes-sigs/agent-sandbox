# Hello World on Kubernetes Sandbox

This document describes how to build a simple "Hello World" Docker image, push it to Google Artifact Registry, and deploy it to a Kubernetes cluster using a custom `Sandbox` resource.

## Prerequisites

1.  **Docker:** Docker installed and running on your local machine.
2.  **gcloud CLI:** Google Cloud SDK installed and configured.
3.  **kubectl:** Kubernetes command-line tool installed.
4.  **Google Cloud Project:** A GCP project with Artifact Registry API enabled.
5.  **Artifact Registry Repository:** A Docker repository created in Artifact Registry.
6.  **Kubernetes Cluster:** Access to a Kubernetes cluster where you have permissions to deploy resources.

## Configuration

Please replace the placeholder values below with your actual environment details:

```bash
export USERNAME=someuser # Replace with your username
export LOCATION=us-central1 # Replace with your Artifact Registry region
export PROJECT=someuser-project # Replace with your GCP Project ID
export REPOSITORY=someuser-repository # Replace with your Artifact Registry repository name
export IMAGE=${LOCATION}-docker.pkg.dev/${PROJECT}/${REPOSITORY}/hello-world:latest # Replace with your Docker image name and tag
```

## Files

*   `Dockerfile`: Defines the instructions to build the Docker image.
*   `hello-world.yaml`: Kubernetes manifest for the `Sandbox` custom resource.

## Steps

**1. Build and Run the Docker Image**
Open a terminal in the directory containing the Dockerfile.

```bash
# Build the image:
docker build -t ${IMAGE} .

# Run the image:
docker run --rm ${IMAGE}
```

**2. Configure Docker Authentication for Artifact Registry**

```bash
gcloud auth configure-docker ${LOCATION}-docker.pkg.dev
```

**3. Tag and Push the Image to Artifact Registry**  

```bash
# Push the image to Artifact Registry.
docker push ${IMAGE}
```

**4. Deploy to Kubernetes**
Ensure your kubectl context is pointing to the correct cluster. Apply the manifest:

```bash
cat hello-world.yaml | envsubst | kubectl apply -f -
```

This will create a Sandbox resource named `hello-world`. The Sandbox controller will then provision the underlying Pod.

**5. Check Sandbox status**

```bash
# Get Sandbox Status
kubectl get sandbox hello-world

# Find all the running pods
kubectl get pods

# Look for the pod which was created
kubectl describe pod hello-world
```

**6. Verify Container Logs**

```bash
# Get Logs: While the pod is running or after it has completed:
kubectl logs hello-world -c my-container
```

You should see the output: `Hello, World from Kubernetes!`
