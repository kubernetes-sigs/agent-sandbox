FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY portal.py .
USER 10000
CMD ["python", "portal.py"]
