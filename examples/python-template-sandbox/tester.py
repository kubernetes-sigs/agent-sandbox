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

def test_health_check(base_url):
    """
    Tests the health check endpoint.
    """
    url = f"{base_url}/"
    try:
        print(f"--- Testing Health Check endpoint ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        response.raise_for_status()
        print("Health check successful!")
        print("Response JSON:", response.json())
        assert response.json()["status"] == "ok"
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during health check: {e}")
        sys.exit(1)

def test_execute(base_url):
    """
    Tests the execute endpoint.
    """
    url = f"{base_url}/execute"
    payload = {"command": "echo 'hello world'"}
    
    try:
        print(f"\n--- Testing Execute endpoint ---")
        print(f"Sending POST request to {url} with payload: {payload}")
        response = requests.post(url, json=payload)
        response.raise_for_status()  # Raise an exception for bad status codes
        
        print("Execute command successful!")
        print("Response JSON:", response.json())
        assert response.json()["stdout"] == "hello world\n"
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during execute command: {e}")
        sys.exit(1)

if __name__ == "__main__":
    if len(sys.argv) != 3:
        print("Usage: python tester.py <server_ip> <server_port>")
        sys.exit(1)
        
    ip = sys.argv[1]
    port = sys.argv[2]
    base_url = f"http://{ip}:{port}"
    
    test_health_check(base_url)
    test_execute(base_url)
