import sys
from huggingface_hub import snapshot_download

if __name__ == "__main__":
    model_names = [
        "Qwen/Qwen3-Embedding-0.6B",
        "distilbert-base-uncased-finetuned-sst-2-english",
    ]

    cache_dir = sys.argv[1] if len(sys.argv) > 1 else None
    
    for model_name in model_names:
        print(f"--- Downloading {model_name} ---")
        try:
            if cache_dir:
                snapshot_download(repo_id=model_name, cache_dir=cache_dir)
            else:
                snapshot_download(repo_id=model_name)
            print(f"Successfully cached {model_name}")
        except Exception as e:
            print(f"Failed to download {model_name}. Error: {e}")

    print("--- Model download process finished. ---")