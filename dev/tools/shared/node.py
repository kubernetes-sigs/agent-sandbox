#!/usr/bin/env python3
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

import os
import platform
import shutil
import subprocess
import tarfile
import urllib.request


def _safe_extractall(tar, install_dir):
    # tarfile filter='data' requires Python 3.11.4+; validate manually instead.
    resolved_root = os.path.realpath(install_dir)
    members = tar.getmembers()

    for member in members:
        if os.path.isabs(member.name):
            raise ValueError(f"Unsafe tar member with absolute path: {member.name!r}")

        if member.isdev():
            raise ValueError(f"Unsafe tar member is a device/FIFO: {member.name!r}")

        resolved_target = os.path.realpath(os.path.join(install_dir, member.name))
        if resolved_target != resolved_root and \
                not resolved_target.startswith(resolved_root + os.sep):
            raise ValueError(f"Unsafe tar member escapes install directory: {member.name!r}")

        if member.issym():
            if os.path.isabs(member.linkname):
                raise ValueError(
                    f"Unsafe symlink with absolute target in {member.name!r}: {member.linkname!r}"
                )
            symlink_dir = os.path.dirname(resolved_target)
            resolved_link = os.path.realpath(os.path.join(symlink_dir, member.linkname))
            if resolved_link != resolved_root and \
                    not resolved_link.startswith(resolved_root + os.sep):
                raise ValueError(
                    f"Unsafe symlink escapes install directory in {member.name!r}: {member.linkname!r}"
                )

        if member.islnk():
            if os.path.isabs(member.linkname):
                raise ValueError(
                    f"Unsafe hardlink with absolute target in {member.name!r}: {member.linkname!r}"
                )
            resolved_link = os.path.realpath(os.path.join(install_dir, member.linkname))
            if resolved_link != resolved_root and \
                    not resolved_link.startswith(resolved_root + os.sep):
                raise ValueError(
                    f"Unsafe hardlink escapes install directory in {member.name!r}: {member.linkname!r}"
                )

        tar.extract(member, path=install_dir)


def install_node(install_dir):
    """Downloads and installs Node.js to the specified directory."""
    print(f"Installing Node.js to {install_dir}...")
    if os.path.exists(install_dir):
        shutil.rmtree(install_dir)
    os.makedirs(install_dir)

    system = platform.system().lower()
    machine = platform.machine().lower()

    if system == "linux":
        os_name = "linux"
    elif system == "darwin":
        os_name = "darwin"
    else:
        print(f"Warning: Unsupported OS for auto-install: {system}")
        return None

    if machine in ["x86_64", "amd64"]:
        arch = "x64"
    elif machine in ["aarch64", "arm64"]:
        arch = "arm64"
    else:
        print(f"Warning: Unsupported architecture for auto-install: {machine}")
        return None

    version = "v22.22.0"
    filename = f"node-{version}-{os_name}-{arch}.tar.gz"
    url = f"https://nodejs.org/dist/{version}/{filename}"

    print(f"Downloading {url}...")
    tar_path = os.path.join(install_dir, filename)
    try:
        urllib.request.urlretrieve(url, tar_path)
    except Exception as e:
        print(f"Failed to download Node.js: {e}")
        return None

    print(f"Extracting {tar_path}...")
    try:
        with tarfile.open(tar_path, "r:gz") as tar:
            _safe_extractall(tar, install_dir)
    except Exception as e:
        print(f"Failed to extract Node.js: {e}")
        return None

    # The tarball creates a subdirectory, e.g., node-v22.13.0-linux-x64
    extracted_dir = os.path.join(install_dir, filename.replace(".tar.gz", ""))
    bin_dir = os.path.join(extracted_dir, "bin")

    if not os.path.exists(bin_dir):
        print(f"Error: expected bin directory not found at {bin_dir}")
        return None

    return bin_dir


def ensure_node(repo_root, env):
    """Ensures Node.js and npm are available, installing locally if needed.

    Updates env["PATH"] if a local installation is used, then resolves the
    full paths of npm and npx via shutil.which so callers can pass them
    directly to subprocess without relying on PATH lookup at exec time.

    Returns:
        (npm_path, npx_path) tuple of absolute paths, or (None, None) on failure.
    """
    # Check if npm is already available in PATH
    npm_path = shutil.which("npm")

    if npm_path:
        print(f"Using system npm: {npm_path}")
    else:
        print("npm not found in PATH.")
        node_install_dir = os.path.join(repo_root, ".node")

        # Check if we already have a local installation
        found_bin = None
        if os.path.exists(node_install_dir):
            for item in os.listdir(node_install_dir):
                candidate = os.path.join(node_install_dir, item, "bin")
                if os.path.isdir(candidate) and os.path.exists(os.path.join(candidate, "node")):
                    found_bin = candidate
                    break

        if found_bin:
            print(f"Using locally installed Node.js at {found_bin}")
            env["PATH"] = f"{found_bin}:{env.get('PATH', '')}"
        else:
            print("Installing Node.js locally...")
            bin_dir = install_node(node_install_dir)
            if not bin_dir:
                print("Failed to install Node.js locally.")
                return None, None
            env["PATH"] = f"{bin_dir}:{env.get('PATH', '')}"

    # Resolve full paths using the (possibly updated) PATH so subprocess
    # callers don't rely on runtime PATH lookup.
    search_path = env.get("PATH", "")
    npm_path = shutil.which("npm", path=search_path)
    npx_path = shutil.which("npx", path=search_path)

    if not npm_path or not npx_path:
        print(f"Error: could not locate npm ({npm_path}) or npx ({npx_path}) in PATH.")
        return None, None

    # Verify the installation
    try:
        node_version = subprocess.run(
            [shutil.which("node", path=search_path) or "node", "--version"],
            env=env, capture_output=True, text=True, check=True,
        )
        npm_version = subprocess.run(
            [npm_path, "--version"],
            env=env, capture_output=True, text=True, check=True,
        )
        print(f"Verified Node.js: {node_version.stdout.strip()}")
        print(f"Verified npm: {npm_version.stdout.strip()}")
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        print(f"Failed to verify Node.js/npm: {e}")
        return None, None

    return npm_path, npx_path
