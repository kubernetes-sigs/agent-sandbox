FROM python:3.11-slim

WORKDIR /app

RUN pip install fastapi uvicorn pydantic

COPY main.py .

RUN chown -R 1000:1000 /app
USER 1000

EXPOSE 8888

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8888", "--log-level", "trace"]
