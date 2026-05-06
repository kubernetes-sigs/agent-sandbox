---
title: "FUSE CSI"
linkTitle: "FUSE CSI"
weight: 25
description: >
  Use Volumes with `Agent Sandbox` and with `FUSE CSI`.
---

# Use volumeClaimTemplates to persist data from Agent Sandbox 

## Prerequisites

- A running GKE cluster with the [Agent Sandbox controller]({{< ref "/docs/getting_started/overview" >}}) installed.
- GKE cluster with [GCS FUSE CSI driver](https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/cloud-storage-fuse-csi-driver) enabled and Workload Identity configured.
- `kubectl` configured to connect to your cluster.
- [Google Cloud CLI](https://cloud.google.com/sdk/docs/install) (`gcloud`) installed and authenticated.

## Steps

1. Specify environment variables:
   ```sh
   export PROJECT_ID="your-gcp-project-id"
   export BUCKET_NAME="your-unique-bucket-name"
   export BUCKET_LOCATION="us-central1"
   export NAMESPACE="default"
   export GSA_NAME="sandbox-gcs-accessor"
   export KSA_NAME="your-sandbox-sa"
   ```

2. Create a GCS Bucket
   ```sh
   gcloud storage buckets create gs://${BUCKET_NAME} \
     --project=$PROJECT_ID \
     --location=$BUCKET_LOCATION \
     --uniform-bucket-level-access
   ```

3. Create a Google Service Account:
   ```sh
   gcloud iam service-accounts create $GSA_NAME \
     --project=$PROJECT_ID \
     --display-name="Service Account for Sandbox GCS Access"
   ```

4. Create access to GCS bucket:
   ```sh
   gcloud storage buckets add-iam-policy-binding gs://$BUCKET_NAME \
     --member="serviceAccount:$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com" \
     --role="roles/storage.objectAdmin"
   ```

5. Create the Kubernetes Service Account
   ```sh
   kubectl -n $NAMESPACE create serviceaccount $KSA_NAME
   ```

6. Bind the GSA and KSA with Workload Identity Federation:
   ```sh
   gcloud iam service-accounts add-iam-policy-binding $GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com \
     --role="roles/iam.workloadIdentityUser" \
     --member="serviceAccount:$PROJECT_ID.svc.id.goog[$NAMESPACE/$KSA_NAME]" \
     --project=$PROJECT_ID
   ```
7. Annotate the Kubernetes Service Account:
   ```sh
   kubectl -n $NAMESPACE annotate serviceaccount $KSA_NAME \
     iam.gke.io/gcp-service-account=$GSA_NAME@$PROJECT_ID.iam.gserviceaccount.com
   ```

8. Create Persistent Volume

   ```sh
   kubectl apply -f - <<EOF
   apiVersion: v1
   kind: PersistentVolume
   metadata:
     name: my-gcs-bucket-pv
     namespace: "${NAMESPACE}"
   spec:
     accessModes:
     - ReadWriteMany
     capacity:
       storage: 10Gi # Must match or exceed the storage requested in the volumeClaimTemplate
     storageClassName: gcsfuse-standard
     mountOptions:
       - implicit-dirs
     csi:
       driver: gcsfuse.csi.storage.gke.io
       # Replace this with your actual Google Cloud Storage bucket name (without the gs:// prefix)
       volumeHandle: "${BUCKET_NAME}"
       readOnly: false
   EOF
   ```

9. Create a sandbox 

   ```sh
   kubectl apply -f - <<EOF
   apiVersion: agents.x-k8s.io/v1alpha1
   kind: Sandbox
   metadata:
     name: sandbox-example
     namespace: "${NAMESPACE}"
   spec:
     podTemplate:
       metadata:
         labels:
           sandbox: my-sandbox
         annotations:
           # REQUIRED: This annotation triggers GKE to inject the GCS FUSE sidecar container
           gke-gcsfuse/volumes: "true"
       spec:
         # REQUIRED: This ServiceAccount must be configured with GKE Workload Identity 
         # and bound to a GCP IAM Role that has access to your bucket (e.g., Storage Object Admin).
         serviceAccountName: "${KSA_NAME}"
         containers:
         - name: my-container
           image: busybox
           command: ["/bin/sh", "-c", "sleep 3600"]
           volumeMounts:
           - name: gcs-pvc
             mountPath: /my-data
     volumeClaimTemplates:
     - metadata:
         name: gcs-pvc
       spec:
         # GCS supports multiple pods reading and writing simultaneously
         accessModes: [ "ReadWriteMany" ]
         # This ensures the claim looks for a PV specifically created for GCS
         storageClassName: "gcsfuse-standard"
         resources:
           requests:
             # Kubernetes requires a size request, even though GCS buckets are virtually limitless
             storage: 10Gi
   EOF
   ```
