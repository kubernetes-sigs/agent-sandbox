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

import unittest
import os
import subprocess
import time
import logging
from agentic_sandbox.extensions.computer_use import ComputerUseSandbox
from kubernetes import client, config

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

class TestComputerUseSandbox(unittest.TestCase):



    def setUp(self):
        """Create the gemini-api-key secret before each test."""
        logging.info("Setting up test...")
        if "GEMINI_API_KEY" not in os.environ:
            logging.error("GEMINI_API_KEY environment variable not set.")
            self.fail("GEMINI_API_KEY environment variable not set.")
        
        logging.info("Creating gemini-api-key secret...")
        subprocess.run(
            f"kubectl create secret generic gemini-api-key --from-literal=key={os.environ['GEMINI_API_KEY']} --dry-run=client -o yaml | kubectl apply -f -", 
            shell=True,
            check=True
        )
        
        logging.info("Waiting for secret to be created...")
        config.load_kube_config()
        core_v1_api = client.CoreV1Api()
        for _ in range(10):
            try:
                core_v1_api.read_namespaced_secret("gemini-api-key", "default")
                logging.info("Secret is ready.")
                return
            except client.ApiException as e:
                if e.status == 404:
                    time.sleep(1)
                else:
                    raise
        raise TimeoutError("Secret did not become ready in time.")

    def test_agent_with_api_key(self):
        """Tests the agent endpoint with a valid API key."""
        logging.info("Starting test_agent_with_api_key...")
        template_name = "sandbox-python-computeruse-template"
        
        with ComputerUseSandbox(template_name, "default", server_port=8080) as sandbox:
            self.assertTrue(sandbox.is_ready())
            logging.info("Sandbox is ready.")
            
            query = "Navigate to https://www.example.com and tell me what the heading says."
            logging.info(f"Sending query: {query}")
            # Pass the API key explicitly
            result = sandbox.agent(query, api_key=os.environ["GEMINI_API_KEY"])
            
            logging.info(f"Received result: {result}")
            self.assertEqual(result.exit_code, 0)
            self.assertIn("Example Domain", result.stdout)
        logging.info("Finished test_agent_with_api_key.")

    def test_agent_without_api_key(self):
        """
        Tests the agent endpoint without a valid API key.
        This test no longer relies on manipulating os.environ locally.
        """
        logging.info("Starting test_agent_without_api_key...")
        template_name = "sandbox-python-computeruse-template"
        
        with ComputerUseSandbox(template_name, "default", server_port=8888) as sandbox:
            self.assertTrue(sandbox.is_ready())
            logging.info("Sandbox is ready.")
            
            query = "what is the weather today"
            logging.info(f"Sending query: {query}")
            # Explicitly omit the API key
            result = sandbox.agent(query, api_key=None)
            
            logging.info(f"Received result: {result}")
            self.assertEqual(result.exit_code, 1)
            # Check for a generic API key-related error message
            self.assertIn("API key", result.stderr, "Expected an API key-related error in stderr")
        logging.info("Finished test_agent_without_api_key.")



if __name__ == "__main__":



    unittest.main()
