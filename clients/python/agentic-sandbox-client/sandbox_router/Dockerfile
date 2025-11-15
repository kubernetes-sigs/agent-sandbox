# Use the official Python image from the Docker Hub as the base image.
FROM python:3.13-slim

WORKDIR /app

# Installation of dependencies for sandbox router.
COPY requirements.txt .

RUN pip install --no-cache-dir --require-hashes -r requirements.txt

COPY sandbox_router.py .

# Change ownership of the /app directory to the non-root user 1000.
RUN chown -R 1000:1000 /app
USER 1000

# Expose the port that the Uvicorn server will run on.
# This must match the port in the CMD instruction below.
EXPOSE 8080

# Use Uvicorn as a production-ready web server to run the Flask app
CMD ["uvicorn", "sandbox_router:app", "--host", "0.0.0.0", "--port", "8080"]
