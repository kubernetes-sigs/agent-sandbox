# A sandboxd runtime image carrying three coding-agent CLIs, so that
# "agtsbx run" can drive them. Build it with:
#
#     docker build -t agtsbx-agents:latest examples/agtsbx-agents

ARG SANDBOXD_IMAGE=us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandboxd:latest-main
FROM ${SANDBOXD_IMAGE} AS sandboxd

# All three agents ship as npm packages, so start from a Node base and copy
# sandboxd in, rather than adding Node to the sandboxd image.
FROM node:22-bookworm-slim

# The agents shell out to git constantly, and need CA certificates to reach
# their model APIs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g \
      opencode-ai \
      @anthropic-ai/claude-code \
      @openai/codex \
    && npm cache clean --force

COPY --from=sandboxd /usr/local/bin/sandboxd /usr/local/bin/sandboxd

# Each agent keeps its config and session state under $HOME. Pointing HOME at
# the sandbox root keeps that state inside the sandbox, where it is discarded
# with it, instead of in a directory the sandbox user cannot write.
ENV HOME=/workspace
RUN mkdir -p /workspace && chown 1000:1000 /workspace

WORKDIR /workspace
# node:22 already defines UID 1000; the numeric form satisfies runAsNonRoot.
USER 1000
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/sandboxd"]
