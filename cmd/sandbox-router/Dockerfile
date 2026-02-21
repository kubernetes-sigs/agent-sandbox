# Build stage
FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod ./
# No go.sum yet as there are no external dependencies.
# COPY go.sum ./

COPY main.go .

RUN CGO_ENABLED=0 GOOS=linux go build -o sandbox-router main.go

# Run stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /app/sandbox-router /sandbox-router

# The deployment manifest might override this with runAsUser: 1000.
# Since it's a static Go binary, it should work fine as any user.
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/sandbox-router"]
