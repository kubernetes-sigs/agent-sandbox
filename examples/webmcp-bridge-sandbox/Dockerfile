FROM mcr.microsoft.com/playwright/python:v1.63.0-noble

WORKDIR /app
COPY pyproject.toml bridge.py entrypoint.sh ./
COPY demo-page ./demo-page
RUN pip install --no-cache-dir -e . && chmod +x entrypoint.sh

ENV TARGET_PAGE_URL=http://localhost:8090/
# pwuser is the non-root user this base image ships for running Chromium;
# switching after the editable install (which needs root to write into
# /usr) and http.server's non-privileged port 8090 doesn't need root either.
USER pwuser
CMD ["./entrypoint.sh"]
