import threading
import time
from datetime import datetime, timedelta, timezone
from kubernetes import client, config

# Configuration
NAMESPACE = "hpa-test"
TEMPLATE = "python-sandbox-template"
RATE_PER_SECOND = 1
TEST_DURATION_MINUTES = 10
CLEANUP_INTERVAL_SECONDS = 30
CLAIM_TTL_SECONDS = 60

# Initialize K8s Client
try:
    config.load_kube_config()
except:
    config.load_incluster_config()

custom_api = client.CustomObjectsApi()

def delete_expired_claims():
    """Background thread to delete claims older than CLAIM_TTL_SECONDS."""
    print("Cleanup thread started...")
    while True:
        try:
            # List all sandbox claims
            claims = custom_api.list_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1alpha1",
                namespace=NAMESPACE,
                plural="sandboxclaims"
            )

            now = datetime.now(timezone.utc)
            for claim in claims.get('items', []):
                creation_ts_str = claim['metadata']['creationTimestamp']
                # K8s timestamps are Zulu/UTC
                creation_ts = datetime.strptime(creation_ts_str, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
                
                if now - creation_ts > timedelta(seconds=CLAIM_TTL_SECONDS):
                    name = claim['metadata']['name']
                    custom_api.delete_namespaced_custom_object(
                        group="extensions.agents.x-k8s.io",
                        version="v1alpha1",
                        name=name,
                        namespace=NAMESPACE,
                        plural="sandboxclaims"
                    )
                    print(f"[Cleanup] Deleted expired claim: {name}")
        except Exception as e:
            print(f"[Cleanup] Error: {e}")
        
        time.sleep(CLEANUP_INTERVAL_SECONDS)

def create_claim(index):
    """Creates a single SandboxClaim."""
    name = f"loadtest-{int(time.time())}-{index}"
    body = {
        "apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
        "kind": "SandboxClaim",
        "metadata": {"name": name, "namespace": NAMESPACE},
        "spec": {"sandboxTemplateRef": {"name": TEMPLATE}}
    }
    try:
        custom_api.create_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sandboxclaims",
            body=body
        )
    except Exception as e:
        print(f"Error creating {name}: {e}")

if __name__ == "__main__":
    # Start the cleanup thread
    cleanup_thread = threading.Thread(target=delete_expired_claims, daemon=True)
    cleanup_thread.start()

    print(f"Starting load test: {RATE_PER_SECOND} claim/sec for {TEST_DURATION_MINUTES}m")
    
    start_time = time.time()
    end_time = start_time + (TEST_DURATION_MINUTES * 60)
    counter = 0

    try:
        while time.time() < end_time:
            loop_start = time.time()
            
            # Fire and forget the creation in a thread to avoid blocking the clock
            threading.Thread(target=create_claim, args=(counter,)).start()
            
            counter += 1
            
            # Precise timing: Calculate how much of our 1-second window is left
            elapsed = time.time() - loop_start
            sleep_time = max(0, (1.0 / RATE_PER_SECOND) - elapsed)
            time.sleep(sleep_time)
            
            if counter % 10 == 0:
                print(f"Progress: {counter} claims created...")

    except KeyboardInterrupt:
        print("Test stopped by user.")

    print(f"Load test complete. Total claims created: {counter}")
    print("Waiting 1 minute for final cleanup...")
    time.sleep(60)