#!/usr/bin/env python3
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

import os
import argparse
import sys
import subprocess
import time

from dev.tools.shared import utils as tools_utils

class TestRunner:
    def __init__(self, name, description):
        self.name = name
        self.description = description
        self.repo_root = self._get_repo_root()
        if self.repo_root not in sys.path:
            sys.path.insert(0, self.repo_root)

        # Pin IMAGE_TAG in the environment to prevent timezone/date drift between setup/build and test execution steps.
        if not os.getenv("IMAGE_TAG"):
            os.environ["IMAGE_TAG"] = tools_utils.get_image_tag()

        self.parser = argparse.ArgumentParser(description=self.description)
        self.parser.add_argument(
            "--image-prefix",
            dest="image_prefix",
            help="prefix for the image name. requires slash at the end if a path",
            type=str,
            default="kind.local/",
        )
        self.parser.add_argument(
            "--skip-initial-deploy",
            dest="skip_initial_deploy",
            help="skip deploying the controller during cluster setup",
            action="store_true",
        )

    def _get_repo_root(self):
        return os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))

    def run_with_retry(self, cmd, attempts=2, retry_delay_seconds=10):
        """Runs a cluster-setup command, retrying on failure.

        Bring-up steps (kind cluster creation, image push, deploys) fail
        intermittently under CI's Docker-in-Docker; most red presubmit runs
        die here before any test executes. Each step is safe to re-run
        (create-kind-cluster is invoked with --recreate and the deploys are
        idempotent applies), so one retry recovers transient failures without
        masking persistent breakage. The delay before retrying gives
        transient daemon states (socket contention, container cleanup races)
        time to clear; an immediate retry tends to hit the same condition.
        """
        if attempts < 1:
            raise ValueError(f"attempts must be >= 1, got {attempts}")
        result = None
        for attempt in range(1, attempts + 1):
            result = subprocess.run(cmd)
            if result.returncode == 0:
                return result
            if attempt < attempts:
                print(
                    f"setup step {os.path.basename(cmd[0])} failed with exit code "
                    f"{result.returncode}; retrying in {retry_delay_seconds}s "
                    f"(attempt {attempt + 1} of {attempts})",
                    file=sys.stderr,
                )
                time.sleep(retry_delay_seconds)
        return result

    def setup_cluster(self, args, extra_push_images_args=None):
        image_tag = os.environ["IMAGE_TAG"]
        result = self.run_with_retry([f"{self.repo_root}/dev/tools/create-kind-cluster", self.name, "--recreate", "--kubeconfig", f"{self.repo_root}/bin/KUBECONFIG"])
        if result.returncode != 0:
            return result

        push_images_cmd = [f"{self.repo_root}/dev/tools/push-images", "--kind-cluster-name", self.name, "--image-prefix", args.image_prefix, "--image-tag", image_tag]
        if extra_push_images_args:
            push_images_cmd.extend(extra_push_images_args)
        result = self.run_with_retry(push_images_cmd)
        if result.returncode != 0:
            return result

        if not getattr(args, "skip_initial_deploy", False):
            result = self.run_with_retry([f"{self.repo_root}/dev/tools/deploy-to-kube", "--image-prefix", args.image_prefix, "--image-tag", image_tag, "--extensions"])
            if result.returncode != 0:
                return result

        result = self.run_with_retry([f"{self.repo_root}/dev/tools/deploy-cloud-provider"])
        return result

    def run_tests(self, args):
        raise NotImplementedError

    def copy_artifacts(self):
        pass

    def main(self):
        args = self.parser.parse_args()
        result = self.setup_cluster(args)
        if result.returncode != 0:
            sys.exit(result.returncode)
        
        result = self.run_tests(args)
        self.copy_artifacts()
        if result:
            sys.exit(result.returncode)
        else:
            sys.exit(0)