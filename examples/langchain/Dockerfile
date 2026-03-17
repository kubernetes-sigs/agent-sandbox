FROM python:3.13-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY coding_agent.py .

RUN mkdir -p /workspace

WORKDIR /workspace

CMD ["python", "/app/coding_agent.py"]