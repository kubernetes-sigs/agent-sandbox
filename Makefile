.PHONY: all
all: fix-go-generate fix-api-docs build lint-go lint-api test-unit toc-verify verify-olm

.PHONY: fix-go-generate
fix-go-generate:
	dev/tools/fix-go-generate

.PHONY: fix-api-docs
fix-api-docs:
	dev/tools/fix-api-docs

.PHONY: install-gen-tools
install-gen-tools:
	dev/tools/fix-go-generate --install-only

GOPATH ?= $(shell go env GOPATH)

PYTHON ?= python3

CRD_REF_DOCS_VERSION := v0.3.0
.PHONY: generate-api-docs
REF_CRD_PATH="./docs/api.md"
generate-api-docs: # Generate API reference documentation
	@echo "Generating API Docs..."
	go install github.com/elastic/crd-ref-docs@$(CRD_REF_DOCS_VERSION)
	$(GOPATH)/bin/crd-ref-docs --source-path=./ --config=./docs/crd-ref-docs.yaml --renderer=markdown --output-path=$(REF_CRD_PATH) --max-depth=10

GOMARKDOC_VERSION := v1.1.0
.PHONY: generate-go-docs
REF_GO_PATH := "./docs/go_sdk_reference.md"
generate-go-docs: # Generate Go SDK reference documentation
	@echo "Generating Go SDK Documentation..."
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@$(GOMARKDOC_VERSION)
	$(GOPATH)/bin/gomarkdoc \
		--repository.url "https://github.com/kubernetes-sigs/agent-sandbox" \
		--repository.default-branch "main" \
		--repository.path "/" \
		./clients/go/sandbox/... > $(REF_GO_PATH).tmp1
	sed 's/^#/##/' < $(REF_GO_PATH).tmp1 > $(REF_GO_PATH).tmp2
	# Strip #L<line> anchors from source links: line numbers are brittle and
	# make the generated docs go stale on every unrelated edit. Keep the
	# file link (per reviewer request). Staged through a temp file (rather than
	# a pipe) so a tail failure aborts the recipe instead of silently writing a
	# partial doc -- portable across /bin/sh implementations without pipefail.
	tail -n +2 < $(REF_GO_PATH).tmp2 > $(REF_GO_PATH).tmp3
	sed -E 's@(/blob/[^)# ]+)#L[0-9]+(-L[0-9]+)?@\1@g' < $(REF_GO_PATH).tmp3 > $(REF_GO_PATH)
	rm $(REF_GO_PATH).tmp1 $(REF_GO_PATH).tmp2 $(REF_GO_PATH).tmp3

PYDOC_MARKDOWN_VERSION := 4.8.2
.PHONY: generate-python-docs
REF_PYTHON_PATH := "./docs/python_sdk_reference.md"
generate-python-docs: # Generate Python SDK reference documentation
	@echo "Generating Python SDK Documentation..."
	$(PYTHON) -m venv .venv
	.venv/bin/python -m pip install --upgrade pip
	.venv/bin/python -m pip install pydoc-markdown==$(PYDOC_MARKDOWN_VERSION)
	.venv/bin/pydoc-markdown -I ./clients/python/agentic-sandbox-client/ -m k8s_agent_sandbox.sandbox_client -m k8s_agent_sandbox.models > $(REF_PYTHON_PATH).tmp1
	sed 's/^#/##/' < $(REF_PYTHON_PATH).tmp1 > $(REF_PYTHON_PATH)
	rm $(REF_PYTHON_PATH).tmp1

VERSION_PKG := sigs.k8s.io/agent-sandbox/internal/version

GIT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "unknown")
GIT_SHA     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE  ?= $(shell dev/tools/build-date)

LD_FLAGS := -s -w -X $(VERSION_PKG).gitVersion=$(GIT_VERSION) \
	-X $(VERSION_PKG).gitSHA=$(GIT_SHA) \
	-X $(VERSION_PKG).buildDate=$(BUILD_DATE)

.PHONY: build
build: build-controller build-sandbox-router build-sandboxd

.PHONY: build-controller
build-controller:
	go build -ldflags "$(LD_FLAGS)" -o bin/manager ./cmd/agent-sandbox-controller

.PHONY: build-sandbox-router
build-sandbox-router:
	go build -ldflags "$(LD_FLAGS)" -o bin/sandbox-router ./sandbox-router/cmd

.PHONY: build-sandboxd
build-sandboxd:
	go build -ldflags "$(LD_FLAGS)" -o bin/sandboxd ./packages/sandboxd/cmd/sandboxd

KIND_CLUSTER=agent-sandbox

# `make deploy-kind` delegates to dev/tools/deploy-kind, which reads its
# configuration from environment variables (GNU make exports command-line
# variables to recipe subprocesses):
#   SKIP_BUILD=true       deploy published images instead of building locally
#   IMAGE_TAG=vX.Y.Z      pin the published image tag (with SKIP_BUILD=true)
#   EXTENSIONS=true       deploy the extensions controller and CRDs
#   CONTROLLER_ONLY=true  build and push only the controller image
#   CONTROLLER_ARGS=...   extra controller flags
#   CONTAINER_ENGINE=...  docker or podman
#   KIND_CLUSTER_NAME=... cluster name (default: agent-sandbox)
.PHONY: deploy-kind
deploy-kind:
	KIND_CLUSTER_NAME="$${KIND_CLUSTER_NAME:-$(KIND_CLUSTER)}" ./dev/tools/deploy-kind

.PHONY: deploy-cloud-provider-kind
deploy-cloud-provider-kind:
	./dev/tools/deploy-cloud-provider

.PHONY: delete-kind
delete-kind:
	kind delete cluster --name ${KIND_CLUSTER}

.PHONY: kill-cloud-provider-kind
kill-cloud-provider-kind:
	killall cloud-provider-kind

.PHONY: test-unit
test-unit:
	./dev/tools/test-unit

.PHONY: test-e2e
test-e2e:
	RACE=$(RACE) ./dev/ci/presubmits/test-e2e

.PHONY: test-e2e-race
test-e2e-race:
	RACE=1 ./dev/ci/presubmits/test-e2e

.PHONY: test-e2e-benchmarks
test-e2e-benchmarks:
	./dev/ci/presubmits/test-e2e --suite benchmarks

.PHONY: test-skill-eval
test-skill-eval:
	./dev/ci/presubmits/test-skill-eval

.PHONY: lint-go
lint-go:
	./dev/tools/lint-go

.PHONY: fix-go
fix-go:
	./dev/tools/lint-go --fix

.PHONY: lint-api
lint-api:
	./dev/tools/lint-api

.PHONY: fix-api
fix-api:
	./dev/tools/lint-api --fix

.PHONY: verify-chart-version
verify-chart-version:
	./dev/tools/verify-chart-version

.PHONY: bump-chart-version
bump-chart-version:
	./dev/tools/bump-chart-version

# Location of your local k8s.io repo (can be overridden: make release-promote TAG=v0.1.0 K8S_IO_DIR=../other/k8s.io)
K8S_IO_DIR ?= ../../kubernetes/k8s.io

# Default remote (can be overridden: make release-publish REMOTE=upstream ...)
REMOTE_UPSTREAM ?= upstream
REMOTE_FORK ?= origin

# Gemini model for release notes generation
GEMINI_MODEL ?= gemini-3.7-flash

# Promote all staging images to registry.k8s.io
# Usage: make release-promote TAG=vX.Y.Z
.PHONY: release-promote
release-promote:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-promote TAG=vX.Y.Z)"; exit 1; fi
	./dev/tools/tag-promote-images --tag=${TAG} --k8s-io-dir=${K8S_IO_DIR} --upstream-remote=${REMOTE_UPSTREAM} --fork-remote=${REMOTE_FORK} $(if $(filter true,$(SKIP_TAGGING)),--skip-tagging) $(if $(filter true,$(ONLY_TAGGING)),--only-tagging)

# Publish a draft release to GitHub
# Usage: make release-publish TAG=vX.Y.Z GEMINI_MODEL=gemini-3.7-flash
.PHONY: release-publish
release-publish: install-gen-tools
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-publish TAG=vX.Y.Z)"; exit 1; fi
	go mod tidy
	PATH="$(CURDIR)/bin:$(PATH)" go generate ./...
	./dev/tools/release --tag=${TAG} --publish --model=${GEMINI_MODEL}

# Generate release manifests only
# Usage: make release-manifests TAG=vX.Y.Z
.PHONY: release-manifests
release-manifests: install-gen-tools
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-manifests TAG=vX.Y.Z)"; exit 1; fi
	go mod tidy
	PATH="$(CURDIR)/bin:$(PATH)" go generate ./...
	./dev/tools/release --tag=${TAG}

# Example usage:
# make release-python-sdk TAG=v0.1.1.post1 (for patch release on PyPI)
# make release-python-sdk TAG=v0.1.0rc1 (for release candidate on PyPI)
.PHONY: release-python-sdk
release-python-sdk:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-python-sdk TAG=vX.Y.Z, TAG=vX.Y.ZrcN, or TAG=vX.Y.Z.postN)"; exit 1; fi
	./dev/tools/release-python --tag=${TAG} --remote=${REMOTE_UPSTREAM}

.PHONY: toc-update
toc-update:
	./dev/tools/update-toc

.PHONY: toc-verify
toc-verify:
	./dev/tools/verify-toc

.PHONY: fix-olm-manifests
fix-olm-manifests:
	./dev/tools/fix-olm-manifests

.PHONY: verify-olm
verify-olm:
	./dev/tools/verify-olm-manifests

.PHONY: verify-olm-bundle
verify-olm-bundle:
	./dev/tools/verify-olm-bundle

.PHONY: clean
clean:
	rm -rf dev/tools/tmp
	rm -rf bin/manager
