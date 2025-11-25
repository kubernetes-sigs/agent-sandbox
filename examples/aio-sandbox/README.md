# All-in-One (AIO) Sandbox Example

This example demonstrates how to create and access an [All-in-One (AIO) Sandbox](https://github.com/agent-infra/sandbox) via Agent-Sandbox.

## Create an AIO Sandbox

Apply the sandbox manifest with AIO Sandbox runtime. You can find the latest version [here](https://github.com/agent-infra/sandbox/pkgs/container/sandbox).

```sh
kubectl apply -f aio-sandbox.yaml
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

The AIO Sandbox has multiple tools pre-installed, including VNC, VSCode, Jupyter and Terminal in one unified environment.

## Accessing the AIO Sandbox Server

Port forward the aio-sandbox server port.

```sh
# set ingress host and port
export INGRESS_HOST="localhost"
export INGRESS_PORT="8080"

# port forward to the aio-sandbox pod
kubectl port-forward --address ${INGRESS_HOST} pod/aio-sandbox-example ${INGRESS_PORT}:8080
```

Setup GATEWAY_URL environment variable:

```sh
export GATEWAY_URL="http://${INGRESS_HOST}:${INGRESS_PORT}"

echo "$GATEWAY_URL"
# http://localhost:8080
```

Connect to the aio-sandbox on a browser via `http://<INGRESS_HOST>:<INGRESS_PORT>`

## Access the AIO Sandbox via Python SDK

```sh
# set up a virtual environment if needed
python3 -m venv venv
source venv/bin/activate

# install the agent-sandbox package
pip install agent-sandbox==0.0.18
```

> **Note:**
> The `agent-sandbox` Python SDK installed above is for controlling tools *inside* the AIO sandbox (e.g., browser, shell, file system). If you want to programmatically provision or manage sandbox infrastructure (Kubernetes resource lifecycle), use the complementary [`agentic-sandbox-client` Python SDK](../../clients/python/agentic-sandbox-client/). These two SDKs can be used together for a complete infrastructure + application workflow.

Run the basic python example:
```sh
python3 main.py
```

## More examples

For more examples of using the AIO Sandbox, refer to agents-infra/sandbox [examples](https://github.com/agent-infra/sandbox/tree/main/examples).

## Cleanup

To remove the sandbox and related resources when you are done, run:

```sh
kubectl delete -f aio-sandbox.yaml

# Cleanup sandbox_screenshot.png from main.py if needed
rm sandbox_screenshot.png
```

## References
- [AIO Sandbox](https://github.com/agent-infra/sandbox/tree/main)
- [AIO Sandbox Python SDK](https://github.com/agent-infra/sandbox/tree/main/sdk/python)