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

"""MovieLens 20M High-Density Agent Sandbox Benchmark.

This benchmark script evaluates Python agent sandbox density under a data-intensive workload
that processes the MovieLens 20M dataset (rating statistics and aggregations using pandas).
It measures execution latency (TTFE/CEL) and memory footprint (max RSS) under GKE Memory Swap.

Required packages: pandas (provided by python-sandbox container).
"""
from contextlib import suppress
import csv
import json
import os
import resource
import sys
import time

DATASET_PATH = "/data/ratings.csv"

def run_benchmark():
    if not os.path.exists(DATASET_PATH):
        raise FileNotFoundError(f"Required MovieLens dataset file not found at {DATASET_PATH}")

    t0 = time.perf_counter()
    import pandas as pd
    print("Executing Pandas DataFrame Ingestion & Analytics...")
    # Load 5,000,000 rows directly into a Pandas DataFrame
    df = pd.read_csv(DATASET_PATH, nrows=5000000)
    count = len(df)
    # Perform realistic Pandas groupby analytical aggregation
    ratings_summary = df.groupby('movieId')['rating'].agg(['mean', 'count'])
    mem_mb = round(df.memory_usage(deep=True).sum() / (1024**2), 2)
    print(f"PANDAS_DATAFRAME_LOADED | Rows: {count} | DataFrame RAM: {mem_mb} MB")

    elapsed = round((time.perf_counter() - t0), 4)
    max_rss_mb = round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 2)

    summary = {
        "sandbox_ttfe_ms": round(elapsed * 1000.0, 2),
        "exec_seconds": elapsed,
        "max_rss_mb": max_rss_mb,
        "rows_processed": count,
        "unique_movies": len(ratings_summary)
    }
    print(json.dumps(summary))
    sys.stdout.flush()

    pid = os.fork()
    if pid == 0:
        # Child process: detach file descriptors & hold Pandas RAM in memory for 10 minutes
        with suppress(OSError):
            os.close(1)
        with suppress(OSError):
            os.close(2)
        time.sleep(600)
        os._exit(0)

if __name__ == "__main__":
    run_benchmark()
