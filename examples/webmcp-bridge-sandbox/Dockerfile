FROM mcr.microsoft.com/playwright/python:v1.63.0-noble

WORKDIR /app
COPY pyproject.toml bridge.py entrypoint.sh ./
COPY demo-page ./demo-page
RUN pip install --no-cache-dir -e . && chmod +x entrypoint.sh

ENV TARGET_PAGE_URL=http://localhost:8090/
CMD ["./entrypoint.sh"]
