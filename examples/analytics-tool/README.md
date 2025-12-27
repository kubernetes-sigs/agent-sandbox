# AI Analytics with agent-sandbox

## Getting Started

### Prerequisites
- Running **GKE** cluster (**Standard** or **Autopilot**))
- `kubectl` access to a Kubernetes **GKE Standard** or **GKE Autopilot** cluster
- Agent-sandbox installed on GKE. Here is the ([Installation Guide](../../getting_started/))

## Deploy analytics tools

In this section we will create our custom Docker image that defines analytics tool for an ADK agent. Then we will push it to our Artifact Registry repository and deploy from it. To do so, follow the steps below.

Run the following commands:

```bash
cd analytics-tool
```

Run this command to create an Artifact Registry repository:

```bash
gcloud artifacts repositories create analytics \
    --project=${PROJECT_ID} \
    --repository-format=docker \
    --location=us \
    --description="Analytics Repo"
```

And now we can create our analytics agent-sandbox tool:

```bash
gcloud builds submit .
```

After build is completed, we can change `<PROJECT_ID>` in `sandbox-python.yaml` and apply it:

```bash
kubectl apply -f sandbox-python.yaml
kubectl apply -f analytics-svc.yaml
```

## Deploy jupyter lab

Now we can deploy a jupyter lab to make some data analytics:

```bash
kubectl apply -f ../jupyterlab.yaml
```

Once it's running, you can port-forward your jupyterlab and access on `http://127.0.0.1:8888` by running this command:

```bash
kubectl port-forward "pod/jupyterlab-sandbox" 8888:8888
```

Now we can follow the `welcome.ipynb` notebook.

## Analytics example

Open `welcome.ipynb` notebook. In the `Download the data` section you can review the dataset that will be used in our example. In the `Data analytics` you can see the actual data analytics. Here we define `analyze_movies` function with the `tool` decorator. Here in the docstring you can observe the instruction for the LLM how to use it. 

The example query looks like this:

```log
Load /my-data/shopping_behavior_updated.csv. This data has 'Purchase Amount (USD)' column. Create a bar chart showing a sum of 'Purchase Amount (USD)' per column 'Location'.
```

The agent will be able to generate code that will be executed in our agent-sandbox pod. For example, the code might look like this:

```python
import pandas as pd
import matplotlib.pyplot as plt
import io
import base64

# Load the data
df = pd.read_csv('/my-data/shopping_behavior_updated.csv')

# Group by 'Location' and sum 'Purchase Amount (USD)'
purchase_amount_by_location = df.groupby('Location')['Purchase Amount (USD)'].sum()

# Create a bar chart
plt.figure(figsize=(10, 6))
purchase_amount_by_location.plot(kind='bar')
plt.title('Total Purchase Amount (USD) by Location')
plt.xlabel('Location')
plt.ylabel('Total Purchase Amount (USD)')
plt.xticks(rotation=45, ha='right')
plt.tight_layout()

# Save to buffer
buf = io.BytesIO()
plt.savefig(buf, format='png')
buf.seek(0)
img_str = base64.b64encode(buf.read()).decode('utf-8')
print(f"<IMG>{img_str}</IMG>")
```

As you can see, in the end the code prints an encoded image. Inside the tool definition we use the regex expression to extract this string, decode, and plot it.

## Cleanup

```bash
gcloud artifacts repositories delete analytics \
    --project=${PROJECT_ID} \
    --location=us
```
