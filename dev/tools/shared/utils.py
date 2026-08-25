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

from datetime import datetime, timezone
import os
import re
import subprocess


# Version strings derived from git refs are interpolated unquoted into the
# Dockerfile's `RUN go build -ldflags="...${GIT_VERSION}..."` instruction. A
# git tag or branch containing shell metacharacters could therefore break out
# of the quoted string and execute arbitrary commands during the build. Allow
# only characters that legitimately appear in git describe/sha output and fail
# closed on anything else.
_SAFE_VERSION_RE = re.compile(r"^[A-Za-z0-9._/-]+$")


def _validate_version_string(value, source):
    """Ensures a git-derived version string is safe to interpolate into a shell
    command, raising ValueError if it contains unexpected characters.
    """
    if not value or not _SAFE_VERSION_RE.match(value):
        raise ValueError(
            f"refusing to use unsafe {source} value {value!r}: only "
            "alphanumerics and the characters '.', '_', '/', '-' are allowed")
    return value


def git_describe():
    """Gets the git describe output for HEAD."""
    raw_version = subprocess.check_output(
        ["git", "describe", "--always", "--dirty"], text=True
    ).strip()
    return _validate_version_string(raw_version, "git describe")


def git_sha():
    """Gets the short git SHA for HEAD."""
    raw_sha = subprocess.check_output(
        ["git", "rev-parse", "--short", "HEAD"], text=True
    ).strip()
    return _validate_version_string(raw_sha, "git sha")


def build_date():
    """Gets the build date in RFC3339 UTC format (%Y-%m-%dT%H:%M:%SZ).

    If BUILD_DATE env var is set, it is returned.
    Otherwise:
    - If the repository is clean, returns the HEAD commit date.
    - If the repository is dirty, returns the mtime of the most recently modified file.
    """
    env_date = os.getenv("BUILD_DATE")
    if env_date:
        return env_date

    repo_root = get_repo_root()

    res = subprocess.run(
        ["git", "status", "--porcelain", "-z"],
        cwd=repo_root,
        capture_output=True,
        check=True,
    )
    raw_status = res.stdout

    if not raw_status:
        # Repo is clean -> return HEAD commit date
        ts_str = subprocess.check_output(
            ["git", "log", "-1", "--format=%ct"],
            cwd=repo_root,
            text=True,
        ).strip()
        dt = datetime.fromtimestamp(int(ts_str), tz=timezone.utc)
        return dt.strftime("%Y-%m-%dT%H:%M:%SZ")

    # Repo is dirty -> find most recently changed dirty file
    entries = raw_status.split(b"\x00")
    mtimes = []
    i = 0
    while i < len(entries):
        entry = entries[i]
        if not entry:
            i += 1
            continue
        status = entry[:2]
        filename = entry[3:].decode("utf-8", errors="replace")
        # If rename or copy (R or C), the next entry in -z output is the old path
        if status[0:1] in (b"R", b"C") or status[1:2] in (b"R", b"C"):
            i += 1  # skip old path
        abs_path = os.path.join(repo_root, filename)
        if os.path.exists(abs_path):
            try:
                mtimes.append(os.path.getmtime(abs_path))
            except OSError:
                pass
        i += 1

    if mtimes:
        latest_dt = datetime.fromtimestamp(max(mtimes), tz=timezone.utc)
        return latest_dt.strftime("%Y-%m-%dT%H:%M:%SZ")

    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def get_image_tag():
    """Gets the image tag from the IMAGE_TAG environment variable, falling back to a
    generated value based on the date and git commit."""
    tag = os.getenv("IMAGE_TAG")
    if tag:
        return tag
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


def kind_env(container_engine, env=None):
    """Return extra env vars for kind when using a non-default container provider.

    kind reads KIND_EXPERIMENTAL_PROVIDER to select its container runtime.
    Only set it for podman (docker is the default).

    When *env* is provided (e.g. a caller-supplied dict), it is copied and the
    provider variable is injected into the copy so the original is never
    mutated.  When *env* is None (the default), a fresh copy of the current
    process environment is used.
    """
    if container_engine == "podman":
        base = (env.copy() if env is not None else os.environ.copy())
        base["KIND_EXPERIMENTAL_PROVIDER"] = "podman"
        return base
    return env  # pass through any caller-supplied env as-is when no injection is needed
