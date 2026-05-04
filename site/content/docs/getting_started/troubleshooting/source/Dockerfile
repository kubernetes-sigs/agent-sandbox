FROM mcr.microsoft.com/playwright/python:v1.58.0-jammy

WORKDIR /app

RUN pip install fastapi uvicorn pydantic

COPY server.py .

RUN chown -R 1000:1000 /app
USER 1000

EXPOSE 8888

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8888", "--log-level", "trace"]
