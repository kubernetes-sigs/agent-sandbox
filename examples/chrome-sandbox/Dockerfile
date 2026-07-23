# Compile our golang binaries in a separate stage to keep the final image small.
# Pin the builder to the build platform and cross-compile to the target arch so the
# Go toolchain always runs natively instead of under QEMU emulation. Running `go build`
# under emulation can crash the Go runtime/GC ("found pointer to free object").
# See https://github.com/golang/go/issues/69255#issuecomment-2523276831
FROM --platform=$BUILDPLATFORM golang:1.26.5 AS builder

# Declare TARGETARCH/TARGETOS to make them available in this build stage.
ARG TARGETARCH
ARG TARGETOS=linux

WORKDIR /src

COPY go.mod go.sum ./
COPY cmd ./cmd

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /chrome-sandbox-entrypoint ./cmd/chrome-sandbox-entrypoint

#----------------------
# Use a base image with a minimal set of packages.
FROM debian:13-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd

#----------------------
# Install general prerequisite packages.
RUN apt-get update && apt-get install -y \
    ca-certificates \
    wget \
    xdg-utils \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*


# Download and install chromium package
# (We previously tried directly install the deb package, but it does not yet appear to be packaged for linux/arm64)
RUN apt-get update && apt-get install --yes --no-install-recommends chromium

# Create a non-root user to run Chrome.
RUN groupadd -r chrome && useradd -r -g chrome -G audio,video chrome \
    && mkdir -p /home/chrome/Downloads && chown -R chrome:chrome /home/chrome

#----------------------
# Install VNC
RUN apt-get update  \
    && apt-get install --yes tigervnc-standalone-server --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# We need xdpyinfo to check that the X server is up
RUN apt-get update  \
    && apt-get install --yes x11-utils --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# Precreate X11 stuff
RUN mkdir -p /tmp/.X11-unix
RUN chmod 1777 /tmp/.X11-unix

#----------------------
# Add our entrypoint script
COPY --chmod=a+rx scripts/start-chrome /start-chrome
COPY --from=builder --chmod=a+rx /chrome-sandbox-entrypoint /chrome-sandbox-entrypoint

#----------------------
# Configure entrypoint, user, etc

WORKDIR /home/chrome

USER chrome

# Set the entrypoint to run our binary.
ENTRYPOINT [ "/chrome-sandbox-entrypoint" ]
