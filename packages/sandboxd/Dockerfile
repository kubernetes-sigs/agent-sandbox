# Stage 1: Build the Go daemon binary
FROM --platform=$BUILDPLATFORM golang:1.26.4 AS builder
ARG TARGETARCH
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ ./api/
COPY cmd/ ./cmd/
COPY controllers/ ./controllers/
COPY extensions/ ./extensions/
COPY internal/ ./internal/
COPY packages/ ./packages/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /sandboxd ./packages/sandboxd/cmd/sandboxd

# Stage 2: Minimal runtime with POSIX utilities for testing ProcessService & PTY
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates procps && rm -rf /var/lib/apt/lists/*
COPY --from=builder /sandboxd /usr/local/bin/sandboxd
WORKDIR /workspace
RUN mkdir -p /workspace && chmod 777 /workspace
ENTRYPOINT ["/usr/local/bin/sandboxd"]
