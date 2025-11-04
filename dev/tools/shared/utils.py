#!/usr/bin/env python3
# Copyright 2025 The Kubernetes Authors
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

from datetime import datetime
import os
import subprocess


def git_describe():
    """Gets the git describe output for HEAD."""
    return subprocess.check_output(
        ["git", "describe", "--always", "--dirty"], text=True).strip()


def get_image_tag():
    """Gets the image tag based on the date and git commit."""
    day = datetime.today().strftime("%Y%m%d")
    return f"v{day}-{git_describe()}"


def get_image_prefix(args):
    """Constructs the image prefix for a container image."""
    if args.image_prefix:
        return args.image_prefix
    raise Exception(f"--image-prefix arg or IMAGE_PREFIX environment variable must be set")


def get_full_image_name(args, image_id, tag=None):
    """Constructs the full GCR image name for an image."""
    image_prefix = get_image_prefix(args)
    if not tag:
        tag = get_image_tag()
    return f"{image_prefix}{image_id}:{tag}"


def get_repo_root():
    """ Gets the absolute path to the repo root directory """
    tools_dir = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))
    return os.path.dirname(os.path.dirname(tools_dir))


def go_tool_args(*args):
    """ Constructs command line arguments to run a go tool """
    repo_root = get_repo_root()
    return ["go", "tool", f"-modfile={repo_root}/dev/tools/go.mod", *args]
