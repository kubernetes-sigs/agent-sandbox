FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY internal ./internal
COPY api ./api
COPY extensions ./extensions
COPY controllers ./controllers
COPY clients ./clients
COPY examples/auto-resume-sandbox/cmd ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -o /sandbox-router ./cmd/sandbox-router/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o /sandbox-suspension-manager ./cmd/suspension-manager/main.go

FROM gcr.io/distroless/static-debian13:nonroot AS state-informer
WORKDIR /
COPY --from=builder /sandbox-router /sandbox-router
USER 65532:65532
ENTRYPOINT ["/sandbox-router"]

FROM gcr.io/distroless/static-debian13:nonroot AS manager
WORKDIR /
COPY --from=builder /sandbox-suspension-manager /sandbox-suspension-manager
USER 65532:65532
ENTRYPOINT ["/sandbox-suspension-manager"]
