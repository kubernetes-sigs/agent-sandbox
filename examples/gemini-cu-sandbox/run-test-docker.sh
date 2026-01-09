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

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

NO_BUILD=false
INTERACTIVE_FLAG=""
DOCKER_RUN_FLAGS="-d"
# Default to non-interactive mode
NON_INTERACTIVE_FLAG="-e NON_INTERACTIVE=1"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --nobuild)
            NO_BUILD=true
            shift
            ;;
        --interactive)
            INTERACTIVE_FLAG="true"
            NON_INTERACTIVE_FLAG=""
            shift
            ;;
        *)
            echo "Unknown parameter passed: $1"
            exit 1
            ;;
    esac
done

if [ -n "$INTERACTIVE_FLAG" ]; then
    DOCKER_RUN_FLAGS="$DOCKER_RUN_FLAGS -e DISPLAY=$DISPLAY -v /tmp/.X11-unix:/tmp/.X11-unix"
fi

if [ "$NO_BUILD" = false ]; then
    echo "Building Docker image..."
    docker build --load -t sandbox-gemini-runtime "$SCRIPT_DIR"
fi

xhost +local:docker
echo "Stopping any existing sandbox-gemini-runtime container..."
docker rm -f sandbox-gemini-runtime || true
echo "Starting sandbox-gemini-runtime container..."
echo "Make sure to set GEMINI_API_KEY environment variable before running the script"
docker run $DOCKER_RUN_FLAGS --rm --name sandbox-gemini-runtime -p 8080:8080 $NON_INTERACTIVE_FLAG -e GEMINI_API_KEY=$GEMINI_API_KEY sandbox-gemini-runtime 

cleanup() {
    echo "Stopping sandbox-runtime container..."
    docker stop sandbox-gemini-runtime
}
trap cleanup EXIT
if [ -n "$INTERACTIVE_FLAG" ]; then
    echo "Running in interactive mode"
    docker exec -it sandbox-gemini-runtime python computer-use-preview/main.py --initial_url "http://www.example.com" --query "Go to YouTube.com, search for funny cats, scroll a few pages down, return the name of the title of a video that has cats in the name."
    
else
    echo "Running in non-interactive mode"
    echo "Waiting for the server to start..."
    while ! curl -s http://127.0.0.1:8080/; do
        sleep 1
    done

    echo "Running the Python tester..."
    python3 "$SCRIPT_DIR/tester.py" "http://127.0.0.1:8080"

    echo "Test finished."
fi
