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