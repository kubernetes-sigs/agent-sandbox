KIND_CLUSTER=kind

.PHONY: all
all: fix-go-generate build

.PHONY: fix-go-generate
fix-go-generate:
	dev/tools/fix-go-generate

.PHONY: build
build:
	go build -o bin/manager cmd/main.go

.PHONY: deploy-kind
deploy-kind:
	kind get clusters | grep ${KIND_CLUSTER} || kind create cluster --name ${KIND_CLUSTER}
	KO_DOCKER_REPO=kind.local ko resolve -f k8s/ -f k8s/crds | kubectl apply -f -

.PHONY: delete-kind
delete-kind:
	kind delete cluster --name ${KIND_CLUSTER}


