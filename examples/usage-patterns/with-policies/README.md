
This example uses a ResourceGraphDefinition (RGD) to define a AgenticSandbox resource.
For more details on RGD please look at [KRO Overview](https://kro.run/docs/overview)

## Install KRO

Follow instructions to [Install KRO](https://kro.run/docs/getting-started/Installation)

## Administrator: Install ResourceGraphDefinition
The administrator installs the RGD in the cluster first before the user can consume it:

```
kubectl apply -f rgd.yaml
```

Validate the RGD is installed correctly:

```
 % kubectl get rgd
NAME              APIVERSION   KIND             STATE    AGE
agentic-sandbox   v1alpha1     AgenticSandbox   Active   6m38s
```

Validate that the new CRD is installed correctly
```
 % kubectl get crd                                                       
NAME                                      CREATED AT
agenticsandboxes.custom.agents.x-k8s.io   2025-09-20T05:03:49Z  # << installed by KRO when reconciling RGD
resourcegraphdefinitions.kro.run          2025-09-20T04:35:37Z  # << installed by KRO
sandboxes.agents.x-k8s.io                 2025-09-19T22:40:05Z  # << installed by agent-sandbox
```

## User: Create AgenticSandbox

The user creates a `AgenticSandbox` resource something like this:

```yaml
apiVersion: custom.agents.x-k8s.io/v1alpha1
kind: AgenticSandbox
metadata:
  name: demo
spec:
  image: nginx
  service:
    port: 80
```

They can then check the status of the applied resource:

```
kubectl get gkeclusters
kubectl get gkeclusters krodemo -n config-connector -o yaml
```

Navigate to GKE Cluster page in the GCP Console and verify the cluster creation.

Once done, the user can delete the `GKECluster` instance:

```
kubectl delete gkecluster krodemo -n config-connector
```
