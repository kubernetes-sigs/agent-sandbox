---
name: python-script
description: "Generate and run Python scripts. Trigger on phrases like 'write a script', 'create a Python program', 'run Python code'."
version: 1.0.0
tags: [python, scripting, automation]
---
# Python Script Generation

## Overview
This skill helps you create, debug, and run Python scripts in the sandbox environment.

## Workflow
1. Create the script file using `write`
2. Execute with `execute("python /app/script.py")`
3. If errors occur, read the file, fix issues, and re-run

## Best Practices
- Always include error handling with try/except
- Use descriptive variable names
- Add comments for complex logic
- Print results for verification

## Common Patterns

### File Processing
```python
import os
for filename in os.listdir('/app/data'):
    with open(f'/app/data/{filename}') as f:
        # process file
```

### Data Transformation
```python
import json
data = json.loads(input_string)
# transform
output = json.dumps(result, indent=2)
```
