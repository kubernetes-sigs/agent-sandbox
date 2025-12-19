# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import requests
import sys
import os

def test_health_check(base_url, headers):
    """
    Tests the health check endpoint.
    """
    url = f"{base_url}/"
    try:
        print(f"--- Testing Health Check endpoint ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url, headers=headers)
        response.raise_for_status()
        print("Health check successful!")
        print("Response JSON:", response.json())
        assert response.json()["status"] == "ok"
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during health check: {e}")
        sys.exit(1)

def test_agent_without_api_key(base_url, headers):
    """
    Tests the agent endpoint without a GEMINI_API_KEY.
    """
    url = f"{base_url}/agent"
    payload = {"query": "what is the weather today"}

    try:
        print(f"\n--- Testing Agent endpoint (without API key) ---")
        print(f"Sending POST request to {url} with payload: {payload}")
        response = requests.post(url, json=payload, headers=headers)
        response.raise_for_status()  # Raise an exception for bad status codes
        
        print("Agent command execution requested successfully!")
        print("Response JSON:", response.json())
        
        response_json = response.json()
        
        # The agent should exit gracefully (exit code 0) even without an API key.
        assert response_json['exit_code'] == 0, f"Expected exit code 0, but got {response_json['exit_code']}"
        
        # The error about the missing API key should be in stdout.
        stdout = response_json['stdout']
        api_key_error_present = (
            "API key not valid" in stdout or
            "GEMINI_API_KEY" in stdout or
            "PERMISSION_DENIED" in stdout or
            "DefaultCredentialsError" in stdout
        )
        assert api_key_error_present, "Expected an API key-related error in stdout, but it was not found."
        
        print("Test successful: Agent exited gracefully with expected API key error message.")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during agent command test (without API key): {e}")
        sys.exit(1)

def test_agent_with_api_key(base_url, headers):
    """
    Tests the agent endpoint with a GEMINI_API_KEY.
    """
    url = f"{base_url}/agent"
    # This query will navigate to a simple page and extract the text.
    payload = {"query": "Navigate to https://www.example.com and tell me what the heading says."}

    try:
        print(f"\n--- Testing Agent endpoint (with API key) ---")
        print(f"Sending POST request to {url} with payload: {payload}")
        response = requests.post(url, json=payload, headers=headers)
        response.raise_for_status()  # Raise an exception for bad status codes
        
        print("Agent command execution requested successfully!")
        print("Response JSON:", response.json())
        
        response_json = response.json()
        
        assert response_json['exit_code'] == 0, f"Expected exit code 0, but got {response_json['exit_code']}"
        
        # The agent's final reasoning should contain the answer.
        stdout = response_json['stdout']
        assert "Example Domain" in stdout, "Expected 'Example Domain' in the agent's output."
        
        print("Test successful: Agent navigated to the page and extracted the heading.")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during agent command test (with API key): {e}")
        sys.exit(1)

if __name__ == "__main__":
    headers = {}
    if len(sys.argv) == 3:
        # Router mode
        print("--- Running in Router Mode ---")
        base_url = sys.argv[1]
        sandbox_id = sys.argv[2]
        headers = {"X-Sandbox-ID": sandbox_id,
                   "X-Sandbox-Namespace": "default",
                   "X-Sandbox-Port": "8080"}
    elif len(sys.argv) == 2:
        # Local Docker mode
        print("--- Running in Local Docker Mode ---")
        base_url = sys.argv[1]
    else:
        print("Usage:")
        print("  Router Mode: python tester.py <router_base_url> <sandbox_id>")
        print("  Local Docker Mode: python tester.py <container_base_url>")
        sys.exit(1)

    test_health_check(base_url, headers)
    print(os.environ.get("GEMINI_API_KEY"))
    if os.environ.get("GEMINI_API_KEY"):
        test_agent_with_api_key(base_url, headers)
    else:
        print("\n--- Skipping test_agent_with_api_key: GEMINI_API_KEY not set ---")
        test_agent_without_api_key(base_url, headers)
