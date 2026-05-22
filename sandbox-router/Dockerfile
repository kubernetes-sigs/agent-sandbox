# Build the ext-proc binary as a static distroless image. Build context
# MUST be the repo root because the binary depends on the shared Go
# module (internal/, api/, etc. via transitive imports).
# dev/tools/push-images is patched to use the repo root as build context
# for this Dockerfile; see the patch in dev/tools/push-images.
FROM --platform=$BUILDPLATFORM golang:1.26.2 AS builder

ARG TARGETARCH
ARG GIT_VERSION=unknown
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

# Only the modules this binary actually imports.
COPY sandbox-router/ ./sandbox-router/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /ext-proc ./sandbox-router/cmd/ext-proc

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /ext-proc /ext-proc
EXPOSE 50051
ENTRYPOINT ["/ext-proc"]
