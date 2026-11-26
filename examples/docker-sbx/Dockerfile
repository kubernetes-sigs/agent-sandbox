FROM ubuntu:24.04

# Pinned for reproducibility; `apt-cache policy docker-sbx` lists versions.
ARG DOCKER_SBX_VERSION=0.46.0-1~ubuntu.24.04~noble

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl \
  && install -m 0755 -d /etc/apt/keyrings \
  && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
  && . /etc/os-release \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
     > /etc/apt/sources.list.d/docker.list \
  && apt-get update \
  && apt-get install -y --no-install-recommends "docker-sbx=${DOCKER_SBX_VERSION}" \
  && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
