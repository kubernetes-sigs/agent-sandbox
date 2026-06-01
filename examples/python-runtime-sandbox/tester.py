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
import urllib.parse

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

def test_list_files(base_url):
    """
    Tests the list files endpoint.
    """
    url = f"{base_url}/list/."
    try:
        print(f"\n--- Testing List Files endpoint ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        response.raise_for_status()
        
        print("List files successful!")
        print("Response JSON:", response.json())
        assert isinstance(response.json(), list)
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during list files: {e}")
        sys.exit(1)

def test_exists(base_url):
    """
    Tests the exists endpoint.
    """
    url = f"{base_url}/exists/."
    try:
        print(f"\n--- Testing Exists endpoint ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        response.raise_for_status()
        
        print("Exists check successful!")
        print("Response JSON:", response.json())
        assert response.json()["path"] == ""
        assert response.json()["exists"] is True

        url = f"{base_url}/exists/does_not_exist"
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        response.raise_for_status()
        
        print("Exists check (negative) successful!")
        print("Response JSON:", response.json())
        assert response.json()["path"] == "does_not_exist"
        assert response.json()["exists"] is False
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during exists check: {e}")
        sys.exit(1)

def test_path_traversal(base_url):
    """
    Tests that relative path traversal attempts are blocked.
    """
    # Try to access /etc/passwd via relative path traversal
    unsafe_path = "../../etc/passwd"
    # We must encode slashes so the web server passes them to the application
    # instead of resolving them itself.
    encoded_path = urllib.parse.quote(unsafe_path, safe='')
    url = f"{base_url}/exists/{encoded_path}"
    
    try:
        print(f"\n--- Testing Path Traversal ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        
        print(f"Response status code: {response.status_code}")
        print("Response JSON:", response.json())
        
        assert response.status_code == 403
        assert response.json()["message"] == "Access denied"
        print("Path traversal blocked successfully!")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during path traversal check: {e}")
        sys.exit(1)

def test_absolute_path_traversal(base_url):
    """
    Tests that absolute path traversal attempts are blocked.
    """
    # Try to access /etc/passwd via absolute path traversal.
    # Note: The server strips leading slashes, effectively re-rooting absolute paths to /app.
    # To test the 'outside /app' check, we must use '..' to traverse up from /app.
    unsafe_path = "/../etc/passwd"
    encoded_path = urllib.parse.quote(unsafe_path, safe='')
    url = f"{base_url}/exists/{encoded_path}"
    
    try:
        print(f"\n--- Testing Absolute Path Traversal ---")
        print(f"Sending GET request to {url}")
        response = requests.get(url)
        
        print(f"Response status code: {response.status_code}")
        print("Response JSON:", response.json())
        
        assert response.status_code == 403
        assert response.json()["message"] == "Access denied"
        print("Absolute path traversal blocked successfully!")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during absolute path traversal check: {e}")
        sys.exit(1)

def test_upload(base_url):
    """
    Tests the upload endpoint with a safe filename, verifies the file
    exists, downloads the content to verify correctness, and cleans up.
    """
    filename = 'test_upload.txt'
    file_content = b'Hello world from upload test'
    
    url_upload = f"{base_url}/upload"
    files = {'file': (filename, file_content)}
    
    try:
        print(f"\n--- Testing Upload endpoint ---")
        print(f"Sending POST request to {url_upload}")
        response = requests.post(url_upload, files=files)
        response.raise_for_status()
        
        print("Upload successful!")
        print("Response JSON:", response.json())
        assert response.status_code == 200
        assert "uploaded successfully" in response.json()["message"]
        
        # 1. Verify file exists
        url_exists = f"{base_url}/exists/{filename}"
        print(f"Checking if file exists via GET {url_exists}")
        response_exists = requests.get(url_exists)
        response_exists.raise_for_status()
        assert response_exists.json()["exists"] is True
        print("File existence verified successfully!")
        
        # 2. Download the file and verify content
        url_download = f"{base_url}/download/{filename}"
        print(f"Downloading file via GET {url_download}")
        response_download = requests.get(url_download)
        response_download.raise_for_status()
        assert response_download.content == file_content
        print("Downloaded file content verified successfully!")
        
        # 3. Clean up the uploaded file
        url_execute = f"{base_url}/execute"
        print(f"Cleaning up uploaded file via POST {url_execute}")
        payload = {"command": f"rm {filename}"}
        response_execute = requests.post(url_execute, json=payload)
        response_execute.raise_for_status()
        print("File cleanup completed successfully!")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during upload verification: {e}")
        sys.exit(1)

def test_upload_path_traversal(base_url):
    """
    Tests that uploading a file with an unsafe filename is blocked.
    """
    url = f"{base_url}/upload"
    # Try to upload using a path traversal filename
    files = {'file': ('../../unsafe_upload.txt', b'malicious payload')}
    
    try:
        print(f"\n--- Testing Upload Path Traversal ---")
        print(f"Sending POST request to {url} with unsafe filename")
        response = requests.post(url, files=files)
        
        print(f"Response status code: {response.status_code}")
        print("Response JSON:", response.json())
        
        assert response.status_code == 403
        assert "Access denied" in response.json()["message"]
        print("Upload path traversal blocked successfully!")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during upload path traversal check: {e}")
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
    test_list_files(base_url)
    test_exists(base_url)
    test_path_traversal(base_url)
    test_absolute_path_traversal(base_url)
    test_upload(base_url)
    test_upload_path_traversal(base_url)
