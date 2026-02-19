#!/usr/bin/env python3
# Copyright 2026 The Kubernetes Authors
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

import subprocess
import sys
import re


def run_command(cmd, cwd=None, capture_output=False, allow_error=False):
    """Helper to run shell commands."""
    try:
        result = subprocess.run(
            cmd,
            cwd=cwd,
            check=True,
            text=True,
            stdout=subprocess.PIPE if capture_output else None,
            stderr=subprocess.PIPE if capture_output else None,
        )
        return result.stdout.strip() if capture_output else None
    except subprocess.CalledProcessError as e:
        if allow_error:
            return None
        # ALWAYS print stderr on failure, even if capture_output was requested
        print(f"❌ Error running command: {' '.join(cmd)}")
        if e.stderr:
            print(f"🛑 Stderr:\n{e.stderr}")
        elif e.stdout:
            print(f"🛑 Stdout:\n{e.stdout}")
        sys.exit(1)


def check_local_repo_state(remote):
    """Ensures the local agent-sandbox repo is clean and up-to-date."""
    print(f"🛡️  Verifying local repository state...")

    # 1. Check for uncommitted changes
    if run_command(["git", "status", "--porcelain"], capture_output=True):
        print(
            "❌ You have uncommitted changes in agent-sandbox. Please commit or stash them."
        )
        sys.exit(1)

    # 2. Fetch upstream
    print(f"⬇️  Fetching {remote}...")
    try:
        run_command(["git", "fetch", remote])
    except subprocess.CalledProcessError:
        print(f"❌ Failed to fetch from '{remote}'. Please check your git remotes.")
        sys.exit(1)

    # 3. Check synchronization
    current_branch = run_command(
        ["git", "rev-parse", "--abbrev-ref", "HEAD"], capture_output=True
    )
    local_sha = run_command(["git", "rev-parse", "HEAD"], capture_output=True)

    if current_branch == "main":
        upstream_sha = run_command(
            ["git", "rev-parse", f"{remote}/main"], capture_output=True
        )
        if local_sha != upstream_sha:
            print(f"❌ Local 'main' is not synced with {remote}/main.")
            print(f"   Local:    {local_sha}")
            print(f"   Upstream: {upstream_sha}")
            print("   Please run: git pull upstream main")
            sys.exit(1)
        print("✅ Local 'main' is up-to-date with upstream.")
    else:
        print(f"⚠️  You are releasing from branch '{current_branch}' (not 'main').")
        print("   Assuming this is a test release. Continuing...")


def check_tag_exists(tag, remote):
    """Check if tag already exists on upstream to prevent overwriting existing releases"""

    print(f"🔍 Checking if tag {tag} already exists on {remote}...")
    remote_tags = run_command(
        ["git", "ls-remote", "--tags", remote, f"refs/tags/{tag}"], capture_output=True
    )
    if remote_tags:
        print(
            f"❌ Tag {tag} already exists on {remote}. Aborting to prevent accidental overwrite."
        )
        print(
            "   If you are resuming a failed run, please manually remove the tag from upstream and retry."
        )
        sys.exit(1)


def create_and_push_tag(tag, remote):
    """Tag creation and push."""

    # Ensure tag exists locally, create if not
    existing_tag = run_command(["git", "tag", "--list", tag], capture_output=True)
    if existing_tag.strip() == tag:
        print(f"✅ Tag {tag} already exists locally.")
    else:
        print(f"➕ Creating tag {tag}...")
        run_command(["git", "tag", "-m", f"Release {tag}", tag])

    # Push tag to upstream
    print(f"⬆️  Ensuring tag {tag} is pushed to {remote}...")
    # This will succeed (exit 0) even if the tag is already there ("Everything up-to-date")
    run_command(["git", "push", remote, tag])
    print("✅ Tag confirmed on upstream.")


def validate_tag(tag):
    """Strictly validates the version against PEP 440."""
    pattern = r"^v?\d+\.\d+\.\d+(?:\.?(?:a|b|rc|post|dev)\d+)?$"

    if not re.match(pattern, tag):
        print(f"❌ Error: Tag '{tag}' is invalid.")
        print("   Allowed examples: v0.1.0, v0.1.0rc1, v0.1.0.post1")
        sys.exit(1)
