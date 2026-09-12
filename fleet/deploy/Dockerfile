# Copyright 2026 The Kubernetes Authors.
# Licensed under the Apache License, Version 2.0.
#
# Build the fleet-member container image — Python, reusing the
# k8s-agent-sandbox SDK instead of reimplementing sandbox operations.
#
# Build context is the agent-sandbox REPO ROOT, not this directory and not
# python/ -- every COPY below is repo-root-relative, because the image needs
# the sibling SDK sources so `pip install` can resolve k8s-agent-sandbox from
# source.
#
#   From fleet/:
#     docker build -t multi-cluster-fleet-member:dev -f deploy/Dockerfile ..
#
#   The `..` context lets us copy both the fleet package and
#   clients/python/agentic-sandbox-client/ (the SDK) in one build.
FROM python:3.12-slim AS build

WORKDIR /src
RUN pip install --no-cache-dir --upgrade pip

# The SDK uses setuptools-scm which infers version from git tags. We're not
# copying .git into the image, so pretend a version to short-circuit the
# lookup. This is the escape hatch documented in the setuptools-scm README.
# Env-var name matches the normalized dist name (`k8s-agent-sandbox` →
# `K8S_AGENT_SANDBOX`) so it applies only to the SDK build, not any of the
# transitive deps.
ENV SETUPTOOLS_SCM_PRETEND_VERSION_FOR_K8S_AGENT_SANDBOX=0.0.0+docker

# Copy the SDK first so pip can install it as a local path dep.
COPY clients/python/agentic-sandbox-client /src/sdk
RUN pip install --no-cache-dir /src/sdk

# Copy the fleet planner package and install it. Deliberately NOT --no-deps:
# the package needs google-cloud-storage, pydantic, pyyaml and kubernetes from
# PyPI. Its k8s-agent-sandbox requirement is unpinned and already satisfied by
# the local install above, so pip resolves it without reaching for PyPI.
COPY fleet/python /src/fleet
RUN pip install --no-cache-dir /src/fleet

# Final runtime image — slim, non-root user.
FROM python:3.12-slim
# Unbuffered stdio so logging.info() shows up in kubectl logs immediately.
# Without this Python's stdout is line-buffered when isatty=False (containers),
# but the various logging framework paths can still buffer at higher levels.
# -u is the belt-and-suspenders version.
ENV PYTHONUNBUFFERED=1
RUN groupadd -r fleet && useradd -r -g fleet -u 1000 fleet
COPY --from=build /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
# ALL THREE console_scripts, not just the default entrypoint. The image is run
# three ways and only one of them uses the ENTRYPOINT below:
#   fleet-member    ENTRYPOINT — the per-cluster capacity/reconcile loop
#   fleetctl        planner-deployment.yaml overrides command to this
#   fleet-hubcheck  hubcheck-job.yaml overrides command to this
# Copying only fleet-member left the other two missing, which fails at
# CONTAINER START, not at build: `exec: "fleetctl": executable file not found`.
# Add a line here whenever pyproject.toml gains a [project.scripts] entry.
COPY --from=build /usr/local/bin/fleet-member   /usr/local/bin/fleet-member
COPY --from=build /usr/local/bin/fleetctl       /usr/local/bin/fleetctl
COPY --from=build /usr/local/bin/fleet-hubcheck /usr/local/bin/fleet-hubcheck
# NUMERIC, not `USER fleet`. The manifests set runAsNonRoot: true, and the
# kubelet enforces that by inspecting the image's user field: a NAME is
# unresolvable to a uid at admission, so the container never starts --
# "image has non-numeric user (fleet), cannot verify user is non-root",
# reported as CreateContainerConfigError, which looks like a bad manifest
# rather than a bad image. Same uid as the useradd above.
USER 1000
ENTRYPOINT ["python", "-u", "-m", "agent_sandbox_fleet.fleet_member"]
