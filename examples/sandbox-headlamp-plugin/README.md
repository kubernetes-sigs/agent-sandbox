# Agent Sandbox for Headlamp

The Agent Sandbox plugin for [Headlamp](https://headlamp.dev/) provides
read-only views of Sandboxes, including status, conditions, events, and links
to backing Pods and Services. It uses the connected user's Kubernetes
permissions. It also shows Claims, WarmPools, and Templates when the API for
each resource is available in every selected cluster.

## Try the plugin

1. Build and load the plugin using the
   [Headlamp plugin setup instructions](https://github.com/headlamp-k8s/plugins/blob/main/agent-sandbox/README.md).
2. Connect Headlamp to a cluster with [Agent Sandbox installed](../../README.md#installation).
3. In the Headlamp sidebar, select **Agent Sandbox** and open **Sandboxes**.

To try the views with sample resources, use the [Hello World Sandbox](../hello-world-sandbox/)
or the [WarmPool quickstart](../warmpool-quickstart/).
