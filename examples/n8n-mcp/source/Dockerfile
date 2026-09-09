FROM python:3.11-slim

WORKDIR /app

RUN pip install --no-cache-dir fastapi uvicorn pydantic python-multipart

COPY server.py .

RUN chown -R 1000:1000 /app
USER 1000

EXPOSE 8888

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8888", "--log-level", "trace"]
