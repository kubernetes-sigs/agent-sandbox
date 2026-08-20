---
applyTo: "clients/python/**/*"
---

# Python SDK Guidelines

- **Python Version**: Enforce Python 3.11+ syntax and idioms.
- **Data Models**: Use Pydantic models for configuration and wire-format structures (`k8s_agent_sandbox.models`); avoid free-form dicts.
- **Sync/Async Parity**: Maintain architectural parity between sync modules and their async siblings (e.g., `sandbox_client.py` vs `async_sandbox_client.py`).
- **Package Naming**: Keep distinct: repo directory `agentic-sandbox-client`, importable package `k8s_agent_sandbox`, PyPI distribution `k8s-agent-sandbox`.
- **Dependencies**: Keep base dependencies minimal (`kubernetes`, `requests`, `pydantic`). Put async dependencies behind `[project.optional-dependencies] async`.
