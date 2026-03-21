---
name: data-analysis
description: "Analyze data files and generate insights. Trigger on phrases like 'analyze this data', 'show statistics', 'summarize the CSV'."
version: 1.0.0
tags: [data, analysis, statistics]
---
# Data Analysis

## Overview
This skill helps analyze data files (CSV, JSON, text) and generate statistical summaries.

## Prerequisites
The sandbox should have Python with pandas available, or use basic Python for simple analysis.

## Workflow
1. Read the data file to understand its structure
2. Write an analysis script
3. Execute and capture results
4. Present findings in a clear format

## Analysis Templates

### CSV Summary
```python
import csv
with open('/app/data.csv') as f:
    reader = csv.DictReader(f)
    rows = list(reader)
    print(f"Rows: {len(rows)}")
    print(f"Columns: {reader.fieldnames}")
```

### JSON Exploration
```python
import json
with open('/app/data.json') as f:
    data = json.load(f)
    print(f"Type: {type(data)}")
    if isinstance(data, list):
        print(f"Items: {len(data)}")
```
