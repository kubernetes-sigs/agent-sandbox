import time
from agentic_sandbox import Sandbox

def main():
    """
    Tests the Sandbox client by creating a sandbox, running a command,
    and then cleaning up.
    """
    template_name = "python-sandbox-template"
    namespace = "default"
    
    print("--- Starting Sandbox Client Test ---")
    
    try:
        with Sandbox(template_name, namespace) as sandbox:
            print("\n--- Testing Command Execution ---")
            command_to_run = "echo 'Hello from the sandbox!'"
            print(f"Executing command: '{command_to_run}'")
            
            result = sandbox.run(command_to_run)
            
            print(f"Stdout: {result.stdout.strip()}")
            print(f"Stderr: {result.stderr.strip()}")
            print(f"Exit Code: {result.exit_code}")
            
            assert result.exit_code == 0
            assert result.stdout.strip() == "Hello from the sandbox!"
            
            print("\n--- Command Execution Test Passed! ---")

            # Test file operations
            print("\n--- Testing File Operations ---")
            file_content = "This is a test file."
            file_path = "test.txt"

            print(f"Writing content to '{file_path}'...")
            sandbox.write(file_path, file_content)

            print(f"Reading content from '{file_path}'...")
            read_content = sandbox.read(file_path).decode('utf-8')

            print(f"Read content: '{read_content}'")
            assert read_content == file_content
            print("--- File Operations Test Passed! ---")

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
        # The __exit__ method of the Sandbox class will handle cleanup.
    finally:
        print("\n--- Sandbox Client Test Finished ---")

if __name__ == "__main__":
    main()
