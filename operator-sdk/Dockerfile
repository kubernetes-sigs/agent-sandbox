# Build the manager binary
FROM quay.io/operator-framework/helm-operator:v1.42.2

ENV HOME=/opt/operator-sdk
COPY operator-sdk/watches.yaml ${HOME}/watches.yaml
COPY helm /opt/helm
WORKDIR ${HOME}
