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

import argparse
import sys
import yaml

def mutate_template(file_path, image):
    with open(file_path, "r") as f:
        data = yaml.safe_load(f)
    
    try:
        # Safely traverse and set the image field
        containers = data["spec"]["podTemplate"]["spec"]["containers"]
        if containers:
            containers[0]["image"] = image
    except (KeyError, TypeError, IndexError) as e:
        print(f"Error parsing/mutating template YAML structure: {e}", file=sys.stderr)
        sys.exit(1)
        
    yaml.safe_dump(data, sys.stdout)

def mutate_router(file_path, image, allow_unauthenticated):
    with open(file_path, "r") as f:
        docs = list(yaml.safe_load_all(f))
        
    mutated = False
    for doc in docs:
        if doc and doc.get("kind") == "Deployment" and doc.get("metadata", {}).get("name") == "sandbox-router-deployment":
            try:
                containers = doc["spec"]["template"]["spec"]["containers"]
                for c in containers:
                    if c.get("name") == "router":
                        if image:
                            c["image"] = image
                        env_mutated = False
                        for env_var in c.get("env", []):
                            if env_var.get("name") == "ALLOW_UNAUTHENTICATED_ROUTER":
                                env_var["value"] = "true" if allow_unauthenticated else "false"
                                env_mutated = True
                        if allow_unauthenticated and not env_mutated:
                            print("Error: ALLOW_UNAUTHENTICATED_ROUTER env var not found in router container", file=sys.stderr)
                            sys.exit(1)
                        mutated = True
            except (KeyError, TypeError) as e:
                print(f"Error parsing/mutating router YAML structure: {e}", file=sys.stderr)
                sys.exit(1)
                
    if not mutated:
        print("Error: sandbox-router-deployment Deployment not found in YAML documents", file=sys.stderr)
        sys.exit(1)
        
    yaml.safe_dump_all(docs, sys.stdout)

def main():
    parser = argparse.ArgumentParser(description="Mutate template and router YAML manifests structurally.")
    subparsers = parser.add_subparsers(dest="command", required=True)
    
    template_parser = subparsers.add_parser("template", help="Mutate SandboxTemplate manifest")
    template_parser.add_argument("file", help="Path to the template YAML file")
    template_parser.add_argument("--image", required=True, help="New image to set in the template container")
    
    router_parser = subparsers.add_parser("router", help="Mutate SandboxRouter manifest")
    router_parser.add_argument("file", help="Path to the router deployment YAML file")
    router_parser.add_argument("--image", help="New image to set in the router container")
    router_parser.add_argument("--allow-unauthenticated", action="store_true", help="Enable unauthenticated router mode")
    
    args = parser.parse_args()
    
    if args.command == "template":
        mutate_template(args.file, args.image)
    elif args.command == "router":
        mutate_router(args.file, args.image, args.allow_unauthenticated)

if __name__ == "__main__":
    main()
