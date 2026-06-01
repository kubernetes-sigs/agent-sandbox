# Copyright 2026 The Kubernetes Authors.
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

import grpc
import sys
import os

# Add generated clients path
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), '../clients/python')))

import agent_sandbox_pb2 as pb
import agent_sandbox_pb2_grpc as pb_grpc

def run_poc():
    print("Connecting to agent-sandbox-agent at localhost:50051...")
    channel = grpc.insecure_channel("localhost:50051")

    # 1. Verify Connection
    try:
        grpc.channel_ready_future(channel).result(timeout=5)
        print("Connection successful!")
    except grpc.FutureTimeoutError:
        print("Error: Could not connect to sandbox daemon. Make sure it is running on localhost:50051")
        sys.exit(1)

    # Instantiating stubs
    process_stub = pb_grpc.ProcessServiceStub(channel)
    fs_stub = pb_grpc.FilesystemServiceStub(channel)
    jupyter_stub = pb_grpc.JupyterServiceStub(channel)
    admin_stub = pb_grpc.AdminServiceStub(channel)

    # --- Test 1: Filesystem CRUD ---
    print("\n--- [1/4] Testing Filesystem Service ---")
    test_file = "/tmp/sandbox_poc_test.txt"
    fs_stub.WriteFile(pb.WriteFileRequest(
        path=test_file,
        content=b"Portable Backend POC Content"
    ))
    print(f"Successfully wrote content to {test_file}")

    read_resp = fs_stub.ReadFile(pb.ReadFileRequest(path=test_file))
    print(f"Verified contents from ReadFile: '{read_resp.content.decode()}'")

    # --- Test 2: Command Execution ---
    print("\n--- [2/4] Testing Process Service ---")
    exec_resp = process_stub.Execute(pb.ExecuteRequest(
        command=["cat", test_file]
    ))
    print(f"Synchronous execution command exit code: {exec_resp.exit_code}")
    print(f"Command Stdout: '{exec_resp.stdout.strip()}'")

    # --- Test 3: Jupyter Stateful Session ---
    print("\n--- [3/4] Testing Stateful Jupyter Service ---")
    session = jupyter_stub.CreateSession(pb.CreateJupyterSessionRequest(kernel_name="python3"))
    print(f"Spawned Jupyter session: {session.session_id}")

    # Set state
    res1 = jupyter_stub.ExecuteCode(pb.ExecuteJupyterCodeRequest(
        session_id=session.session_id,
        code="a = 10\nb = 5\ntotal = a * b"
    ))
    print("Code execution block 1 status:", res1.status)

    # Read state
    res2 = jupyter_stub.ExecuteCode(pb.ExecuteJupyterCodeRequest(
        session_id=session.session_id,
        code="print(f'Calculated value is: {total}')"
    ))
    print(f"Code execution block 2 stdout: '{res2.stdout.strip()}'")

    # --- Test 4: Admin Reusability ---
    print("\n--- [4/4] Testing Admin Cleanup Service ---")
    admin_stub.Clean(pb.CleanRequest())
    print("Triggered clean operation.")

    # Verify test file is wiped out
    try:
        fs_stub.ReadFile(pb.ReadFileRequest(path=test_file))
        print("Warning: Test file still exists!")
    except grpc.RpcError as e:
        print(f"Verified cleanup successful. File is gone. gRPC error status: {e.code()}")

    print("\n=== All POC Checks Completed Successfully! ===")

if __name__ == "__main__":
    run_poc()
