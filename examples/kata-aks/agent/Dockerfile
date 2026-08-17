FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1

WORKDIR /app

RUN groupadd --gid 1000 appuser \
    && useradd --uid 1000 --gid 1000 --create-home --shell /usr/sbin/nologin appuser \
    && chown appuser:appuser /app

COPY --chown=1000:1000 requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY --chown=1000:1000 agent.py .

EXPOSE 8080

USER appuser

CMD ["uvicorn", "agent:app", "--host", "0.0.0.0", "--port", "8080"]
