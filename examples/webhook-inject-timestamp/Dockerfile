# docker build -t us-docker.pkg.dev/<project-id>/<repo-name>/webhook-image:latest .
# docker push us-docker.pkg.dev/<project-id>/<repo-name>/webhook-image:latest

# Build go binary
FROM golang:1.26.2 AS builder

WORKDIR /workspace

# Copy the source code
COPY main.go ./

# Alternatively run `go mod tidy` locally, copy go.sum in and use download instead of tidy command.
# go.sum is not included to reduce the amount of unnecessary files in the examples directory.
# COPY go.mod go.sum ./
# RUN go mod download
COPY go.mod ./
RUN go mod tidy

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /webhook main.go

# Use a minimal base image
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /webhook /webhook

ENTRYPOINT ["/webhook"]
