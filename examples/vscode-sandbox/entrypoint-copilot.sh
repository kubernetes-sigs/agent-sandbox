#!/bin/bash
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

set -x

# Install gh CLI if not present
if ! command -v gh &> /dev/null; then
  echo "Installing GitHub CLI..."
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    | tee /etc/apt/sources.list.d/github-cli.list > /dev/null
  apt-get update -qq && apt-get install -y -qq gh
fi

# Authenticate gh CLI using the mounted GH_TOKEN.
#
# NOTE: To use `gh copilot`, the token must include the "copilot" scope.
# Generate a token with the correct scopes:
#   gh auth token  (does NOT include copilot scope by default)
#
# Instead, create a personal access token at https://github.com/settings/tokens
# with the "copilot" scope, or run the following on your local machine:
#   gh auth refresh --scopes copilot
#   gh auth token
#
# Then create the secret:
#   kubectl create secret generic gh-copilot-token --from-literal=token=<YOUR_TOKEN>
if [ -n "$GH_TOKEN" ]; then
  echo "$GH_TOKEN" | gh auth login --with-token
  echo "GitHub CLI authenticated"
  gh auth status

  # Install Copilot CLI binary (auto-accept the install prompt)
  echo "Y" | gh copilot > /dev/null 2>&1 || true
  echo "Copilot CLI ready"
else
  echo "Warning: GH_TOKEN not set, GitHub CLI will not be available"
fi

# Start code-server
/usr/bin/code-server --auth=none --bind-addr=0.0.0.0:13337
