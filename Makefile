.PHONY: all
all: fix-go-generate build lint-go test-unit toc-verify

.PHONY: fix-go-generate
fix-go-generate:
	dev/tools/fix-go-generate

.PHONY: build
build:
	go build -o bin/manager cmd/agent-sandbox-controller/main.go

KIND_CLUSTER=agent-sandbox

.PHONY: deploy-kind
deploy-kind:
	./dev/tools/create-kind-cluster --recreate ${KIND_CLUSTER} --kubeconfig bin/KUBECONFIG
	./dev/tools/push-images --image-prefix=kind.local/ --kind-cluster-name=${KIND_CLUSTER}
	./dev/tools/deploy-to-kube --image-prefix=kind.local/

	@if [ "$(EXTENSIONS)" = "true" ]; then \
		echo "🔧 Patching controller to enable extensions..."; \
		kubectl patch statefulset agent-sandbox-controller \
			-n agent-sandbox-system \
			-p '{"spec": {"template": {"spec": {"containers": [{"name": "agent-sandbox-controller", "args": ["--extensions=true"]}]}}}}'; \
	fi

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

.PHONY: lint-go
lint-go:
	./dev/tools/lint-go

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
	ifndef TAG
		$(info ❌ ERROR: TAG is undefined.)
		$(info )
		$(info Usage Examples:)
		$(info    • Release: 					make release-python-sdk TAG=v0.1.1)
		$(info    • Patch Release:        		make release-python-sdk TAG=v0.1.1.post1)
		$(info    • Release to TestPyPI only:	make release-python-sdk TAG=v0.1.1rc1)
		$(info )
		$(error 🛑 Aborting release)
	endif
		./dev/tools/release-python --tag=${TAG} --remote=${REMOTE_UPSTREAM}

.PHONY: toc-update
toc-update:
	./dev/tools/update-toc

.PHONY: toc-verify
toc-verify:
	./dev/tools/verify-toc