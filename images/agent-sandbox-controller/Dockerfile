# Build go binaries
# See https://github.com/golang/go/issues/69255#issuecomment-2523276831
FROM --platform=$BUILDPLATFORM golang:1.25.1 AS builder
ARG TARGETARCH

ADD go.mod /workspace/
ADD go.sum /workspace/
ADD api/ /workspace/api/
ADD cmd/ /workspace/cmd/
ADD controllers/ /workspace/controllers/

WORKDIR /workspace

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /agent-sandbox-controller ./cmd/


# The controller image
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /agent-sandbox-controller /agent-sandbox-controller

ENTRYPOINT ["/agent-sandbox-controller"]