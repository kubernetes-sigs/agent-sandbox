# Build go binaries
# See https://github.com/golang/go/issues/69255#issuecomment-2523276831
FROM --platform=$BUILDPLATFORM golang:1.25.7 AS builder

# Declare TARGETARCH to make it available in this build stage
ARG TARGETARCH

WORKDIR /workspace

# Download dependencies first to leverage layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the code source needed
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY controllers/ ./controllers/
COPY extensions/ ./extensions/
COPY internal/ ./internal/

# Build the binary with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /agent-sandbox-controller ./cmd/agent-sandbox-controller


# The controller image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /agent-sandbox-controller /agent-sandbox-controller

ENTRYPOINT ["/agent-sandbox-controller"]
