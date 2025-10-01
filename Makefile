.PHONY: all
all: fix-go-generate build

.PHONY: fix-go-generate
fix-go-generate:
	dev/tools/fix-go-generate

.PHONY: build
build:
	go build -o bin/manager cmd/agent-sandbox-controller/main.go

KIND_CLUSTER=agent-sandbox

.PHONY: deploy-kind
deploy-kind:
	kind get clusters | grep ${KIND_CLUSTER} || kind create cluster --name ${KIND_CLUSTER}
	./dev/tools/push-images --image-prefix=kind.local/ --kind-cluster-name=${KIND_CLUSTER}
	./dev/tools/deploy-to-kube --image-prefix=kind.local/

.PHONY: delete-kind
delete-kind:
	kind delete cluster --name ${KIND_CLUSTER}

.PHONY: test-unit
test-unit:
	./dev/tools/test-unit
