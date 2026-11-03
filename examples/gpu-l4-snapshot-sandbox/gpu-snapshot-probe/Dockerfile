FROM python:3.12-slim
# The [ctk] extra installs the CUDA runtime libraries as wheels. GKE mounts
# the NVIDIA driver (libcuda) into the container at /usr/local/nvidia.
RUN pip install --no-cache-dir "cupy-cuda12x[ctk]==14.2.0"
ENV LD_LIBRARY_PATH=/usr/local/nvidia/lib64
RUN useradd --uid 10001 --create-home probe
COPY server.py /server.py
USER 10001
EXPOSE 8888
CMD ["python", "/server.py"]
