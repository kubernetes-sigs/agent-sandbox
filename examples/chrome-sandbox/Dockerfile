# Compile our golang binaries in a separate stage to keep the final image small.
FROM golang:1.25.7 AS builder

WORKDIR /src

COPY go.mod go.sum ./
COPY cmd ./cmd

RUN CGO_ENABLED=0 go build -o /chrome-sandbox-entrypoint ./cmd/chrome-sandbox-entrypoint

#----------------------
# Use a base image with a minimal set of packages.
FROM debian:13-slim

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
