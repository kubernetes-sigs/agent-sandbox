FROM python:3.12-slim
RUN useradd --uid 10001 --create-home probe && mkdir /state && chown probe:probe /state
COPY server.py /server.py
USER 10001
EXPOSE 8888
CMD ["python", "/server.py"]
