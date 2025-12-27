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
# `EXTENSIONS=true make deploy-kind` to deploy with Extensions enabled.
deploy-kind:
	./dev/tools/create-kind-cluster --recreate ${KIND_CLUSTER} --kubeconfig bin/KUBECONFIG
	./dev/tools/push-images --image-prefix=kind.local/ --kind-cluster-name=${KIND_CLUSTER}
	@if [ "$(EXTENSIONS)" = "true" ]; then \
		echo "🚀 Deploying with extensions enabled..."; \
		./dev/tools/deploy-to-kube --image-prefix=kind.local/ --extensions; \
	else \
		echo "🚀 Deploying without extensions..."; \
		./dev/tools/deploy-to-kube --image-prefix=kind.local/; \
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
