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

# Build stage
FROM golang:1.26.3 AS builder

WORKDIR /workspace

# Copy the Go modules manifests
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire project source
COPY . .

# Build tool-agent and tool-sudo binaries
RUN CGO_ENABLED=0 GOOS=linux go build -o /tool-agent ./examples/sandboxed-tools/cmd/tool-agent
RUN CGO_ENABLED=0 GOOS=linux go build -o /tool-sudo ./examples/sandboxed-tools/cmd/tool-sudo

# Final runtime stage
FROM debian:trixie-slim

# Install basic packages and utility tools
RUN apt-get update && apt-get install --yes \
    curl \
    ca-certificates \
    util-linux \
    && rm -rf /var/lib/apt/lists/*

# Create the non-root clawtainer user (UID 1000)
RUN useradd -u 1000 -m -s /bin/bash clawtainer

# Copy the compiled binaries into standard system locations
COPY --from=builder /tool-agent /tool-agent
COPY --from=builder /tool-sudo /usr/bin/sudo

# Make sure binaries have correct executable permissions
RUN chmod +x /tool-agent && chmod +x /usr/bin/sudo

# Run tool-agent as the primary PID 1 process running as root (UID 0)
ENTRYPOINT ["/tool-agent"]
