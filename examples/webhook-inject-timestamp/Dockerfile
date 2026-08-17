# docker build -t us-docker.pkg.dev/<project-id>/<repo-name>/webhook-image:latest .
# docker push us-docker.pkg.dev/<project-id>/<repo-name>/webhook-image:latest

# Build go binary
FROM golang:1.26.2 AS builder

WORKDIR /workspace

# Download dependencies first to take advantage of Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY main.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /webhook main.go

# Use a minimal base image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /webhook /webhook

ENTRYPOINT ["/webhook"]
