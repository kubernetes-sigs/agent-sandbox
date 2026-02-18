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
from k8s_agent_sandbox.extensions.computer_use import ComputerUseSandbox
from kubernetes import client, config

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

class TestComputerUseSandbox(unittest.TestCase):

    def setUp(self):
        """Create the gemini-api-key secret before each test."""
        logging.info("Setting up test...")
        if not os.environ.get("GEMINI_API_KEY"):
            logging.error("GEMINI_API_KEY environment variable not set or is empty.")
            self.fail("GEMINI_API_KEY environment variable not set or is empty.")
        
        logging.info("Creating gemini-api-key secret using Python client...")
        secret_name = "gemini-api-key"
        api_key_value = os.environ['GEMINI_API_KEY']
        
        # Base64 encode the API key for Kubernetes secret
        import base64
        encoded_api_key = base64.b64encode(api_key_value.encode("utf-8")).decode("utf-8")

        secret_body = client.V1Secret(
            api_version="v1",
            kind="Secret",
            metadata=client.V1ObjectMeta(name=secret_name),
            data={"key": encoded_api_key}
        )
        
        try:
            core_v1_api = client.CoreV1Api()
            core_v1_api.create_namespaced_secret(namespace="default", body=secret_body)
            logging.info(f"Secret {secret_name} created successfully in namespace 'default'.")
        except client.ApiException as e:
            if e.status == 409: # Already exists
                logging.warning(f"Secret {secret_name} already exists in namespace 'default'. Attempting to update it.")
                core_v1_api.replace_namespaced_secret(name=secret_name, namespace="default", body=secret_body)
                logging.info(f"Secret {secret_name} updated successfully in namespace 'default'.")
            else:
                logging.error(f"Error creating/updating secret: {e}", exc_info=True)
                raise
        
        logging.info("Waiting for secret to be created...")

def load_kubernetes_config():
    """Loads Kubernetes configuration, prioritizing kubeconfig and falling back to an environment variable."""
    try:
        config.load_kube_config()
        logging.info("Kubernetes config loaded from kubeconfig file.")
    except config.ConfigException:
        logging.info("Kubeconfig file not found, attempting to load from environment variable.")
        try:
            config.load_kube_config(config_file=os.getenv("KUBECONFIG_FILE"))
            logging.info("Kubernetes config loaded from KUBECONFIG_FILE environment variable.")
        except Exception as e:
            logging.error(f"Could not load Kubernetes config: {e}", exc_info=True)
            raise

class TestComputerUseSandbox(unittest.TestCase):

    @classmethod
    def setUpClass(cls):
        """Create the gemini-api-key secret before all tests."""
        logging.info("Setting up test class...")
        if not os.environ.get("GEMINI_API_KEY"):
            logging.error("GEMINI_API_KEY environment variable not set or is empty.")
            raise unittest.SkipTest("GEMINI_API_KEY environment variable not set or is empty. Skipping tests.")
        
        load_kubernetes_config()
        
        logging.info("Creating gemini-api-key secret using Python client...")
        secret_name = "gemini-api-key"
        api_key_value = os.environ['GEMINI_API_KEY']
        
        # Base64 encode the API key for Kubernetes secret
        import base64
        encoded_api_key = base64.b64encode(api_key_value.encode("utf-8")).decode("utf-8")

        secret_body = client.V1Secret(
            api_version="v1",
            kind="Secret",
            metadata=client.V1ObjectMeta(name=secret_name),
            data={"key": encoded_api_key}
        )
        
        cls.core_v1_api = client.CoreV1Api()
        try:
            cls.core_v1_api.create_namespaced_secret(namespace="default", body=secret_body)
            logging.info(f"Secret {secret_name} created successfully in namespace 'default'.")
        except client.ApiException as e:
            if e.status == 409: # Already exists
                logging.warning(f"Secret {secret_name} already exists in namespace 'default'. Attempting to update it.")
                cls.core_v1_api.replace_namespaced_secret(name=secret_name, namespace="default", body=secret_body)
                logging.info(f"Secret {secret_name} updated successfully in namespace 'default'.")
            else:
                logging.error(f"Error creating/updating secret: {e}", exc_info=True)
                raise
        
        logging.info("Waiting for secret to be created in namespace 'default'...")
        for i in range(10):
            try:
                cls.core_v1_api.read_namespaced_secret("gemini-api-key", "default")
                logging.info("Secret is ready.")
                return
            except client.ApiException as e:
                if e.status == 404:
                    logging.info(f"Secret not found, retry {i+1}/10...")
                    time.sleep(1)
                else:
                    logging.error(f"Error reading secret: {e}", exc_info=True)
                    raise
        raise TimeoutError("Secret did not become ready in time.")

    @classmethod
    def tearDownClass(cls):
        """Delete the gemini-api-key secret after all tests."""
        logging.info("Tearing down test class...")
        try:
            cls.core_v1_api.delete_namespaced_secret(name="gemini-api-key", namespace="default")
            logging.info("Secret 'gemini-api-key' deleted successfully from namespace 'default'.")
        except client.ApiException as e:
            if e.status == 404:  # Not found
                logging.info("Secret 'gemini-api-key' was already deleted or not found in namespace 'default'.")
            else:
                logging.error(f"Error deleting secret: {e}", exc_info=True)
                raise

    def test_agent_with_api_key(self):
        """Tests the agent endpoint with a valid API key."""
        logging.info("Starting test_agent_with_api_key...")
        template_name = "sandbox-python-computeruse-template"
        
        with ComputerUseSandbox(template_name, "default", server_port=8080) as sandbox:
            self.assertTrue(sandbox.is_ready())
            logging.info("Sandbox is ready.")
            
            query = "Navigate to https://www.example.com and tell me what the heading says."
            logging.info(f"Sending query: {query}")
            result = sandbox.agent(query)
            
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

        # Delete the secret that setUpClass created to ensure the sandbox starts without it.
        logging.info("Deleting secret for 'without_api_key' test...")
        core_v1_api = client.CoreV1Api()
        try:
            core_v1_api.delete_namespaced_secret(name="gemini-api-key", namespace="default")
            logging.info("Secret 'gemini-api-key' deleted for this test.")
        except client.ApiException as e:
            if e.status == 404:
                logging.warning("Secret 'gemini-api-key' was not found for deletion, which is expected in some cases.")
            else:
                logging.error(f"Error deleting secret for test: {e}")
                raise

        template_name = "sandbox-python-computeruse-template"

        with self.assertRaises(TimeoutError) as cm:
            with ComputerUseSandbox(template_name, "default", server_port=8080) as sandbox:
                pass # Should not reach here
        self.assertIn("Sandbox did not become ready within", str(cm.exception))

        logging.info("Attempting to create sandbox without API key, expecting failure.")
        # The agent call would also fail if the sandbox were to become ready,
        # but the test is designed to verify the sandbox doesn't become ready
        # without the API key, leading to a TimeoutError.
        logging.info("Finished test_agent_without_api_key.")



if __name__ == "__main__":



    unittest.main()
