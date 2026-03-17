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
import sys
from pathlib import Path
from transformers import AutoTokenizer, AutoModelForCausalLM

def download_model():
    MODEL_ID = "Salesforce/codegen-350M-mono"
    CACHE_DIR = Path("/models")
    HF_TOKEN = os.getenv("HF_TOKEN")
    
    if not HF_TOKEN:
        print("ERROR: HF_TOKEN environment variable not set")
        print("Set it with: export HF_TOKEN='your_token_here'")
        sys.exit(1)
    
    print(f"Cache directory: {CACHE_DIR}")
    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    print(f"Cache directory ready: {CACHE_DIR}")
    
    print(f"\nDownloading tokenizer for {MODEL_ID}...")
    try:
        tokenizer = AutoTokenizer.from_pretrained(
            MODEL_ID,
            token=HF_TOKEN,
            trust_remote_code=True,
            cache_dir=str(CACHE_DIR)
        )
        print("Tokenizer downloaded successfully")
    except Exception as e:
        print(f"Failed to download tokenizer: {e}")
        sys.exit(1)
    
    print(f"\nDownloading model {MODEL_ID}...")
    print("This will take several minutes...")
    try:
        model = AutoModelForCausalLM.from_pretrained(
            MODEL_ID,
            token=HF_TOKEN,
            trust_remote_code=True,
            cache_dir=str(CACHE_DIR),
            device_map=None,
            low_cpu_mem_usage=True
        )
        print("Model downloaded successfully")
    except Exception as e:
        print(f"Failed to download model: {e}")
        sys.exit(1)
    
    print(f"\nVerifying model files in {CACHE_DIR}...")
    model_files = list(CACHE_DIR.rglob("*"))
    total_size = sum(f.stat().st_size for f in model_files if f.is_file())
    total_size_gb = total_size / (1024**3)
    
    print(f"Model cache contains {len(model_files)} files")
    print(f"Total size: {total_size_gb:.2f} GB")
    print(f"\nSUCCESS! Model cached at: {CACHE_DIR}")
    print(f"\nTo use in your deployment, mount this directory as a volume:")
    print(f"hostPath: {CACHE_DIR}")

if __name__ == "__main__":
    print("=" * 80)
    print("HuggingFace Model Downloader")
    print("=" * 80)
    
    try:
        download_model()
    except KeyboardInterrupt:
        print("\n\nDownload interrupted")
        sys.exit(1)
    except Exception as e:
        print(f"\nUnexpected error: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)