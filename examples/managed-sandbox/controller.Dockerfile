# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /managed-sandbox-controller ./cmd/controller

FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /managed-sandbox-controller /managed-sandbox-controller

ENTRYPOINT ["/managed-sandbox-controller"]
