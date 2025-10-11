#!/bin/bash
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


set -e

echo "Building Docker image..."
docker build -t sandbox-runtime .

echo "Starting sandbox-runtime container..."
# Run the container in detached mode and publish the port
docker run -d --rm --name sandbox-runtime -p 8888:8888 sandbox-runtime

# Cleanup function to ensure the container is stopped
cleanup() {
    echo "Stopping sandbox-runtime container..."
    docker stop sandbox-runtime
}

# Trap EXIT signal to call the cleanup function when the script ends
trap cleanup EXIT

echo "Waiting for the server to start..."
sleep 5 # Give the server a moment to initialize

echo "Running the Python tester..."
python3 tester.py 127.0.0.1 8888

echo "Test finished."
# The 'trap' command will now execute the cleanup function to stop the container
