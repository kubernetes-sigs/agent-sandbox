# Use the official Python image from the Docker Hub as the base image.
FROM python:3.11-slim

WORKDIR /app

RUN apt update && apt install curl zip unzip -y && apt clean

# Installation of dependencies for python runtime sandbox.
COPY requirements.txt .

RUN pip install --no-cache-dir --require-hashes -r requirements.txt

COPY main.py .

# Change ownership of the /app directory to the non-root user 1000.
RUN chown -R 1000:1000 /app
USER 1000

# Expose the port that the Uvicorn server will run on.
# This must match the port in the CMD instruction below.
EXPOSE 8888

# The command to run when the container starts.
# This starts the Uvicorn server, making our API available.
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8888", "--log-level", "trace"]
