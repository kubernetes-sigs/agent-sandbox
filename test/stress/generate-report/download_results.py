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
import json
import ssl
import sys
import urllib.request
import urllib.error
from pathlib import Path

# List of expected files to try downloading from the GCS stress-test/ directory.
EXPECTED_FILES = [
    "summary.json",
    "metrics.jsonl",
    "sandboxes.jsonl",
    "timeseries.jsonl",
    "watch.jsonl",
    "controller.log",
    "pods.txt",
    "nodes.txt",
    "top-nodes.txt"
]

def normalize_url(url):
    url = url.rstrip('/')
    
    if url.startswith('gs://'):
        path = url[5:]
        return f"https://storage.googleapis.com/{path}"
        
    if 'gcsweb.k8s.io/gcs/' in url:
        idx = url.find('gcsweb.k8s.io/gcs/')
        path = url[idx + len('gcsweb.k8s.io/gcs/'):]
        return f"https://storage.googleapis.com/{path}"
        
    if url.startswith('https://storage.googleapis.com/'):
        return url
        
    raise ValueError(f"Unsupported URL format: {url}")

def make_ssl_context():
    # Some Python installs (notably python.org builds on macOS) ship without
    # access to the system trust store; prefer certifi's CA bundle when available.
    ctx = ssl.create_default_context()
    try:
        import certifi
        ctx.load_verify_locations(certifi.where())
    except ImportError:
        pass
    return ctx

SSL_CONTEXT = make_ssl_context()

def download_file(url, out_path):
    print(f"Downloading {url} ...")
    try:
        with urllib.request.urlopen(url, timeout=60, context=SSL_CONTEXT) as response:
            data = response.read()
    except urllib.error.HTTPError as e:
        if e.code in (403, 404):
            print(f"File not available (HTTP {e.code}), skipping.")
            return False
        raise
    except (urllib.error.URLError, TimeoutError) as e:
        # Keep going: a stalled connection or transient network error for one
        # artifact should not lose every artifact after it.
        print(f"WARNING: download failed ({e}), skipping.", file=sys.stderr)
        return False

    # GZIP detection based on magic bytes (0x1f, 0x8b).
    # jsonl artifacts are stored gzipped but without the .gz suffix; restore
    # it so downstream readers pick the right decoder. Other formats (e.g.
    # .pprof) are gzip-compressed by definition and keep their name.
    is_gzip = len(data) >= 2 and data[0] == 0x1f and data[1] == 0x8b
    final_path = out_path
    if is_gzip and out_path.suffix == '.jsonl':
        final_path = out_path.with_name(out_path.name + '.gz')
        print(f"-> Detected GZIP magic bytes. Restoring extension: {final_path.name}")

    final_path.write_bytes(data)
    print(f"-> Saved to {final_path}")
    return True

def main():
    parser = argparse.ArgumentParser(description="Download stress test artifacts from GCS and restore .gz extensions.")
    parser.add_argument("--url", required=True, help="GCS folder URL (gs://... or https://gcsweb.k8s.io/... or https://storage.googleapis.com/...)")
    parser.add_argument("--output-dir", required=True, help="Local directory to write downloaded artifacts")
    args = parser.parse_args()

    try:
        base_url = normalize_url(args.url)
    except ValueError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    print(f"Normalized GCS URL: {base_url}")
    print(f"Output directory: {output_dir}\n")

    # Clear known artifacts from earlier runs so a file missing from this run
    # (or present under the other .gz/plain variant) cannot leak stale data
    # from a previous download into the report.
    for filename in EXPECTED_FILES:
        for stale in (output_dir / filename, output_dir / (filename + '.gz')):
            stale.unlink(missing_ok=True)

    downloaded = 0
    for filename in EXPECTED_FILES:
        file_url = f"{base_url}/{filename}"
        out_file_path = output_dir / filename
        if download_file(file_url, out_file_path):
            downloaded += 1

    # CPU profile names depend on the phases that ran, so derive them from
    # summary.json. Older runs used a .pb suffix; save either as .pprof.
    summary_path = output_dir / "summary.json"
    if summary_path.exists():
        with open(summary_path) as f:
            raw_phases = json.load(f).get("phases", [])
        # Current schema: a list of {"name": ...}; older runs used a
        # name-keyed dict or a plain list of names.
        if isinstance(raw_phases, dict):
            phases = list(raw_phases.keys())
        else:
            phases = [p.get("name", "") if isinstance(p, dict) else str(p) for p in raw_phases]
        for phase in [p for p in phases if p]:
            out_file_path = output_dir / f"pprof-apiserver-{phase}.pprof"
            for candidate in (f"pprof-apiserver-{phase}.pprof", f"pprof-apiserver-{phase}.pb"):
                if download_file(f"{base_url}/{candidate}", out_file_path):
                    downloaded += 1
                    break

    print(f"\nCompleted downloading {downloaded} files successfully.")

if __name__ == "__main__":
    main()
