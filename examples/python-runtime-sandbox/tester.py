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

import time
import requests
import sys
import concurrent.futures

def test_health_check(base_url):
    """
    Tests the health check endpoint.
    """
    url = f"{base_url}/"
    max_retries = 10
    for i in range(max_retries):
        try:
            print(f"--- Testing Health Check endpoint (Attempt {i+1}/{max_retries}) ---")
            print(f"Sending GET request to {url}")
            response = requests.get(url)
            response.raise_for_status()
            print("Health check successful!")
            print("Response JSON:", response.json())
            assert response.json()["status"] == "ok"
            return
        except (requests.exceptions.RequestException, AssertionError) as e:
            print(f"Attempt {i+1} failed: {e}")
            if i < max_retries - 1:
                time.sleep(2)
            else:
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

def test_execute_stateful(base_url):
    """
    Tests the execute_stateful endpoint.
    """
    url = f"{base_url}/execute_command_stateful"
    
    try:
        print(f"\n--- Testing Execute stateful endpoint ---")
        
        # 1. Define a variable
        payload1 = {"code": "x = 42"}
        print(f"Sending POST request to {url} with payload: {payload1}")
        response = requests.post(url, json=payload1)
        response.raise_for_status()
        
        # 2. Print the variable to verify persistence
        payload2 = {"code": "print(x + x)"}
        print(f"Sending POST request to {url} with payload: {payload2}")
        response = requests.post(url, json=payload2)
        response.raise_for_status()
        
        print("Execute stateful command successful!")
        print("Response JSON:", response.json())
        assert "84" in response.json()["stdout"]
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during execute stateful: {e}")
        sys.exit(1)

def test_execute_stateful_long_running(base_url):
    """
    Tests that we can run code longer than the default timeout by specifying a custom timeout.
    """
    url = f"{base_url}/execute_command_stateful"
    
    try:
        print(f"\n--- Testing Execute stateful Long Running (Custom Timeout) ---")
        
        # Sleep for 15s, but ask for a 30s timeout. This should SUCCEED.
        payload = {"code": "import time; time.sleep(15); print('Finished sleeping')", "timeout": 30}
        print(f"Sending POST request to {url} with payload: {payload}")
        
        response = requests.post(url, json=payload)
        response.raise_for_status()
        
        print("Execute stateful long-running command successful!")
        print("Response JSON:", response.json())
        assert "Finished sleeping" in response.json()["stdout"]
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during execute stateful long running: {e}")
        sys.exit(1)

def test_execute_stateful_timeout(base_url):
    """
    Tests the execute_stateful endpoint with a long-running command.
    """
    url = f"{base_url}/execute_command_stateful"
    
    try:
        print(f"\n--- Testing Execute stateful Timeout ---")
        
        # Sleep for 15 seconds. The server has a 10s timeout for collecting output.
        payload = {"code": "import time; time.sleep(15); print('Should not see this')"}
        print(f"Sending POST request to {url} with payload: {payload}")
        
        response = requests.post(url, json=payload)
        response.raise_for_status()
        
        print("Execute stateful timeout command successful!")
        print("Response JSON:", response.json())
        
        # Verify that we didn't get the output because of the timeout
        assert "Should not see this" not in response.json()["stdout"]
        assert response.json()["stderr"] == "Execution timed out after 10 seconds due to Kernel inactivity."
        assert response.json()["exit_code"] == 1
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during execute stateful timeout: {e}")
        sys.exit(1)

def test_execute_stateful_concurrent(base_url):
    """
    Tests concurrent execution requests to ensure the server handles them safely (serialized).
    """
    url = f"{base_url}/execute_command_stateful"
    print(f"\n--- Testing Execute stateful Concurrent ---")

    def run_request(code, expected):
        try:
            res = requests.post(url, json={"code": code})
            res.raise_for_status()
            return expected in res.json()["stdout"]
        except Exception as e:
            print(f"Request failed: {e}")
            return False

    try:
        # We use a ThreadPool to send requests "at the same time"
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
            # Req 1: Sleeps for 2 seconds
            future1 = executor.submit(run_request, "import time; time.sleep(2); print('req1')", "req1")
            # Req 2: Immediate print
            future2 = executor.submit(run_request, "print('req2')", "req2")

            assert future1.result() is True
            assert future2.result() is True
        
        print("Concurrent execution test passed!")
    except (AssertionError, Exception) as e:
        print(f"An error occurred during concurrent execution test: {e}")
        sys.exit(1)

def test_execute_stateful_interruption(base_url):
    """
    Tests that a timed-out command actually stops the kernel (interrupts it),
    allowing subsequent commands to run immediately.
    """
    url = f"{base_url}/execute_command_stateful"
    print(f"\n--- Testing Execute stateful Interruption ---")

    try:
        # 1. Run a command that sleeps for 10s, but timeout after 1s.
        # If the bug exists, the kernel will keep sleeping for 9 more seconds.
        print("Sending long-running command (should timeout)...")
        requests.post(url, json={"code": "import time; time.sleep(10)", "timeout": 1})

        time.sleep(2)  # Wait a moment to ensure the kernel has processed the interruption
        # 2. Then send a follow-up command. If the kernel was properly interrupted, this should return immediately.
        # If the kernel was successfully interrupted, this should return instantly.
        print("Sending follow-up command (should succeed immediately)...")
        start = time.time()
        resp = requests.post(url, json={"code": "print('recovered')"})
        code_execution_duration = time.time() - start
        
        resp.raise_for_status()
        assert "recovered" in resp.json()["stdout"]
        assert code_execution_duration < 2, f"Kernel took too long to recover: {code_execution_duration}s"
        
        print("Interruption test passed! Kernel recovered immediately.")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during interruption test: {repr(e)}")
        sys.exit(1)

def test_upload_download(base_url):
    """
    Tests the upload and download endpoints.
    """
    try:
        print(f"\n--- Testing Upload/Download endpoints ---")
        
        # Upload
        upload_url = f"{base_url}/upload"
        file_content = b"Hello Sandbox!"
        files = {'file': ('test.txt', file_content)}
        
        print(f"Uploading file to {upload_url}")
        response = requests.post(upload_url, files=files)
        response.raise_for_status()
        print("Upload successful!")
        
        # Download
        download_url = f"{base_url}/download/test.txt"
        print(f"Downloading file from {download_url}")
        response = requests.get(download_url)
        response.raise_for_status()
        
        print("Download successful!")
        assert response.content == file_content
        print("File content matches!")
        
    except (requests.exceptions.RequestException, AssertionError) as e:
        print(f"An error occurred during upload/download: {e}")
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
    test_execute_stateful(base_url)
    test_execute_stateful_long_running(base_url)
    test_execute_stateful_concurrent(base_url)
    test_execute_stateful_interruption(base_url)
    test_execute_stateful_timeout(base_url)
    test_upload_download(base_url)
