
# AIO Sandbox Example

This example demonstrates how to create and access an [AIO Sandbox](https://github.com/agent-infra/sandbox) via Agent-Sandbox.

## Create an AIO Sandbox

Apply the sandbox manifest with AIO Sandbox runtime.

```sh
kubectl apply -k base
# sandbox.agents.x-k8s.io/aio-sandbox-example created
```

They can then check the status of the applied resource.
Verify sandbox and pod are running:

```sh
# wait until the sandbox is ready
kubectl wait --for=condition=Ready sandbox aio-sandbox-example

kubectl get sandbox
# NAME                  AGE
# aio-sandbox-example   41s
kubectl get pod aio-sandbox-example
# NAME                  READY   STATUS    RESTARTS   AGE
# aio-sandbox-example   1/1     Running   0          49s
```

## Accessing the AIO Sandbox Server

Port forward the aio-sandbox server port.

```sh
kubectl port-forward --address 0.0.0.0 pod/aio-sandbox-example 8080
```

Connect to the aio-sandbox on a browser via http://localhost:8080 or <machine-dns>:8080


## Access the AIO Sandbox via Python SDK

```sh
# set up a virtual environment if needed
python3 -m venv venv
source venv/bin/activate

# install the agent-sandbox package
pip install agent-sandbox
```

Run the basic python example:
```sh
python3 main.py
```

Run the site to markdown example:

```sh
# install the playwright package
pip install playwright

python3 site_to_markdown.py
```