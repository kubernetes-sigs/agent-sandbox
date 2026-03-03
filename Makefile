.PHONY: all
all: fix-go-generate build lint-go lint-api test-unit toc-verify

.PHONY: fix-go-generate
fix-go-generate:
	dev/tools/fix-go-generate

.PHONY: build
build:
	go build -o bin/manager cmd/agent-sandbox-controller/main.go

KIND_CLUSTER=agent-sandbox

.PHONY: deploy-kind
# `EXTENSIONS=true make deploy-kind` to deploy with Extensions enabled.
# `CONTROLLER_ARGS="--enable-pprof-debug --zap-log-level=debug" make deploy-kind` to deploy with custom controller flags.
# `CONTROLLER_ONLY=true make deploy-kind` to build and push only the controller image.
deploy-kind:
	./dev/tools/create-kind-cluster --recreate ${KIND_CLUSTER} --kubeconfig bin/KUBECONFIG
	./dev/tools/push-images --image-prefix=kind.local/ --kind-cluster-name=${KIND_CLUSTER} $(if $(filter true,$(CONTROLLER_ONLY)),--controller-only)
	./dev/tools/deploy-to-kube --image-prefix=kind.local/ $(if $(filter true,$(EXTENSIONS)),--extensions) $(if $(CONTROLLER_ARGS),--controller-args="$(CONTROLLER_ARGS)")

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
	./dev/ci/presubmits/test-e2e

.PHONY: test-e2e-benchmarks
test-e2e-benchmarks:
	./dev/ci/presubmits/test-e2e --suite benchmarks

.PHONY: lint-go
lint-go:
	./dev/tools/lint-go

.PHONY: lint-api
lint-api:
	./dev/tools/lint-api

# Location of your local k8s.io repo (can be overridden: make release-promote TAG=v0.1.0 K8S_IO_DIR=../other/k8s.io)
K8S_IO_DIR ?= ../../kubernetes/k8s.io

# Default remote (can be overriden: make release-publish REMOTE=upstream ...)
REMOTE_UPSTREAM ?= upstream

# Promote all staging images to registry.k8s.io
# Usage: make release-promote TAG=vX.Y.Z
.PHONY: release-promote
release-promote:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-promote TAG=vX.Y.Z)"; exit 1; fi
	./dev/tools/tag-promote-images --tag=${TAG} --k8s-io-dir=${K8S_IO_DIR}

# Publish a draft release to GitHub
# Usage: make release-publish TAG=vX.Y.Z
.PHONY: release-publish
release-publish:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-publish TAG=vX.Y.Z)"; exit 1; fi
	go mod tidy
	go generate ./...
	./dev/tools/release --tag=${TAG} --publish

# Generate release manifests only
# Usage: make release-manifests TAG=vX.Y.Z
.PHONY: release-manifests
release-manifests:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-manifests TAG=vX.Y.Z)"; exit 1; fi
	go mod tidy
	go generate ./...
	./dev/tools/release --tag=${TAG}

# Example usage:
# make release-python-sdk TAG=v0.1.1rc1 (to release only on TestPyPI, blocked from PyPI in workflow)
# make release-python-sdk TAG=v0.1.1.post1 (for patch release on TestPyPI and PyPI)
.PHONY: release-python-sdk
release-python-sdk:
	@if [ -z "$(TAG)" ]; then echo "TAG is required (e.g., make release-python-sdk TAG=vX.Y.Z.postN)"; exit 1; fi
	./dev/tools/release-python --tag=${TAG} --remote=${REMOTE_UPSTREAM}

.PHONY: toc-update
toc-update:
	./dev/tools/update-toc

.PHONY: toc-verify
toc-verify:
	./dev/tools/verify-toc

.PHONY: clean
clean:
	rm -rf dev/tools/tmp
	rm -rf bin/manager

IMG ?= agent-sandbox-controller:latest

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Versions
KUSTOMIZE_VERSION ?= v5.4.3
OPERATOR_SDK_VERSION ?= v1.39.2
GOLANGCI_LINT_VERSION ?= v2.1.0

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
KUBECTL ?= $(LOCALBIN)/kubectl
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
CONTAINER_TOOL ?= docker

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
    $(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef


# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.3.10

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
IMAGE_TAG_BASE ?= agent-sandbox

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION)

.PHONY: manifests
manifests: ## Generate CRDs and RBAC into config/crd/bases and config/rbac (via codegen.go).
	go generate ./codegen.go

.PHONY: bundle
# `EXTENSIONS=true make bundle` to include --extensions in the bundle Deployment (same idea as deploy-kind).
bundle: manifests kustomize operator-sdk ## Generate OLM bundle (operator-sdk + kustomize). Overwrites config/manifests/bases/*.clusterserviceversion.yaml.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG) \
	$(if $(filter true,$(EXTENSIONS)),&& $(KUSTOMIZE) edit add patch --path extensions-args.patch.yaml,)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle
	$(if $(filter true,$(EXTENSIONS)),cd config/manager && $(KUSTOMIZE) edit remove patch --path extensions-args.patch.yaml,)

.PHONY: bundle-build
bundle-build: bundle ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) $(CONTAINER_TOOL)-push IMG=$(BUNDLE_IMG)