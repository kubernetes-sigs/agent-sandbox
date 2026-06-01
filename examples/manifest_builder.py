from kubernetes import client, config
import sys

def create_sandbox_claim_resource(name, namespace, template_name):
    # Load K8s cluster configuration
    try:
        config.load_kube_config()
    except Exception:
        config.load_incluster_config()
        
    custom_api = client.CustomObjectsApi()
    
    # Construct unstructured SandboxClaim payload
    # This follows the updated Persona A best practice of using Claims + Templates
    sandbox_claim_manifest = {
        "apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
        "kind": "SandboxClaim",
        "metadata": {
            "name": name, 
            "namespace": namespace,
            "labels": {
                "agents.x-k8s.io/template-name": template_name
            }
        },
        "spec": {
            "sandboxTemplateRef": {
                "name": template_name
            }
        }
    }
    
    # Deploy to GKE
    try:
        custom_api.create_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=namespace,
            plural="sandboxclaims",
            body=sandbox_claim_manifest
        )
        print(f"Successfully created SandboxClaim: {name}")
    except client.exceptions.ApiException as e:
        if e.status == 409:
            print(f"SandboxClaim {name} already exists.")
        else:
            print(f"Error creating SandboxClaim: {e}")
            sys.exit(1)

if __name__ == "__main__":
    create_sandbox_claim_resource(
        name="python-dynamic-claim", 
        namespace="default", 
        template_name="python-sandbox-template-2"
    )
