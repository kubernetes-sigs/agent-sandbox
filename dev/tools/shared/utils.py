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

import os
import subprocess


def get_git_commit_short():
    """Gets the short git commit hash for HEAD."""
    return subprocess.check_output(["git", "rev-parse", "--short", "HEAD"], text=True).strip()


def get_image_tag():
    """Gets the image tag based on the git commit."""
    return f"git-{get_git_commit_short()}"


def get_image_prefix(args):
    """Constructs the image prefix for a container image."""
    if args.image_prefix:
        return args.image_prefix
    raise Exception(f"--image-prefix arg or IMAGE_PREFIX environment variable must be set")


def get_full_image_name(args, image_id):
    """Constructs the full GCR image name for an image."""
    image_prefix = get_image_prefix(args)
    tag = get_image_tag()
    return f"{image_prefix}{image_id}:{tag}"


def update_env_with_go_work_off(kwargs):
    """ Returns a copy of kwargs with GOWORK=off in the env """
    env = os.environ.copy()
    env["GOWORK"] = "off"
    if "env" in kwargs:
        env.update(kwargs["env"])
    kwargs["env"] = env
    return kwargs

def run_go_command(*args, **kwargs):
  """ Runs a Go command with GOWORK=off """
  return subprocess.run(*args, **update_env_with_go_work_off(kwargs))

def check_output_go_command(*args, **kwargs):
  """ Runs a Go command with GOWORK=off """
  return subprocess.check_output(*args, **update_env_with_go_work_off(kwargs))

def check_go_command(*args, **kwargs):
  """ Runs a Go command with GOWORK=off """
  subprocess.check_call(*args, **update_env_with_go_work_off(kwargs))

def get_repo_root():
    """ Gets the absolute path to the repo root directory """
    tools_dir = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))
    return os.path.dirname(os.path.dirname(tools_dir))


def go_tool_args(*args):
    """ Constructs command line arguments to run a go tool """
    repo_root = get_repo_root()
    return ["go", "tool", f"-modfile={repo_root}/dev/tools/go.mod", *args]
