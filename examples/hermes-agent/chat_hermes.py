import urllib.request
import json

def chat_with_hermes(prompt):
    url = "http://localhost:8642/v1/chat/completions"
    headers = {"Content-Type": "application/json"}
    data = {
        "model": "hermes-2-pro-mistral-7b",
        "messages": [{"role": "user", "content": prompt}]
    }
    
    try:
        req = urllib.request.Request(url, data=json.dumps(data).encode(), headers=headers)
        with urllib.request.urlopen(req, timeout=10) as response:
            if response.status == 200:
                res_data = json.loads(response.read().decode())
                return res_data['choices'][0]['message']['content']
            else:
                return f"Error: Received status code {response.status}"
    except Exception as e:
        return f"Connection failed: {e}. Is port-forwarding running?"

def main():
    print("====================================================")
    print("Hermes Agent Chat")
    print("Make sure to run: kubectl port-forward pod/<pod-name> 8642:8642")
    print("Type 'exit' or 'quit' to stop.")
    print("====================================================")
    
    while True:
        try:
            user_input = input("\nYou: ")
            if user_input.lower() in ['exit', 'quit']:
                print("Goodbye!")
                break
            if not user_input.strip():
                continue
                
            response = chat_with_hermes(user_input)
            print(f"\nHermes: {response}")
        except KeyboardInterrupt:
            print("\nGoodbye!")
            break
        except Exception as e:
            print(f"\nAn error occurred: {e}")

if __name__ == "__main__":
    main()
