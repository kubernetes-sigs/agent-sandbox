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

.PHONY: delete-kind
delete-kind:
	kind delete cluster --name ${KIND_CLUSTER}

.PHONY: test-unit
test-unit:
	./dev/tools/test-unit

.PHONY: test-e2e
test-e2e:
	./dev/ci/presubmits/test-e2e

.PHONY: lint-go
lint-go:
	./dev/tools/lint-go

# Example usage: make release TAG=v0.1.0
.PHONY: release
release:
	go mod tidy
	go generate ./...
	./dev/tools/release --tag=${TAG}

.PHONY: toc-update
toc-update:
	./dev/tools/update-toc

.PHONY: toc-verify
toc-verify:
	./dev/tools/verify-toc