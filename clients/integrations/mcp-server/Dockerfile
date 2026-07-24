# Use a lightweight Python base image
FROM python:3.12-slim

# Prevent python from buffering stdout/stderr
ENV PYTHONUNBUFFERED=1

# Set the working directory
WORKDIR /app

# Create a non-root user and group
RUN groupadd -r appuser && useradd -r -g appuser appuser

# Copy the project configuration first to leverage Docker layer caching
COPY --chown=appuser:appuser pyproject.toml README.md /app/

# Copy the application code
COPY --chown=appuser:appuser k8s_agent_sandbox_mcp_server /app/k8s_agent_sandbox_mcp_server

RUN pip install --no-cache-dir .

USER appuser

# Expose the port uvicorn will run on
EXPOSE 8000

# Run the MCP server via uvicorn using the app instance from server.py
ENTRYPOINT ["uvicorn", "k8s_agent_sandbox_mcp_server.app:app", "--host", "0.0.0.0", "--port", "8000"]
