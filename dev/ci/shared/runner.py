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

from dev.tools.shared import utils as tools_utils

class TestRunner:
    def __init__(self, name, description):
        self.name = name
        self.description = description
        self.repo_root = self._get_repo_root()
        if self.repo_root not in sys.path:
            sys.path.insert(0, self.repo_root)

        self.parser = argparse.ArgumentParser(description=self.description)
        self.parser.add_argument(
            "--image-prefix",
            dest="image_prefix",
            help="prefix for the image name. requires slash at the end if a path",
            type=str,
            default="kind.local/",
        )

    def _get_repo_root(self):
        return os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))

    def setup_cluster(self, args, extra_push_images_args=None):
        image_tag = tools_utils.get_image_tag()
        result = subprocess.run([f"{self.repo_root}/dev/tools/create-kind-cluster", self.name, "--recreate", "--kubeconfig", f"{self.repo_root}/bin/KUBECONFIG"])
        if result.returncode != 0:
            return result
        
        push_images_cmd = [f"{self.repo_root}/dev/tools/push-images", "--kind-cluster-name", self.name, "--image-prefix", args.image_prefix, "--image-tag", image_tag]
        if extra_push_images_args:
            push_images_cmd.extend(extra_push_images_args)
        result = subprocess.run(push_images_cmd)
        if result.returncode != 0:
            return result

        result = subprocess.run([f"{self.repo_root}/dev/tools/deploy-to-kube", "--image-prefix", args.image_prefix, "--image-tag", image_tag])
        if result.returncode != 0:
            return result

        result = subprocess.run([f"{self.repo_root}/dev/tools/deploy-cloud-provider"])
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