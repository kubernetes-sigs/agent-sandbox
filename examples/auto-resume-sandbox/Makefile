# Makefile for Envoy-Based Auto-Resume Ingress Interception PoC

.PHONY: build docker-build kind-load deploy deploy-envoy test clean

IMAGE_NAME ?= sandbox-state-informer:latest
KIND_CLUSTER ?= agent-sandbox

build:
	go build -o ../../bin/sandbox-state-informer ./cmd/state-informer/main.go
	go build -o ../../bin/sandbox-suspension-manager ./cmd/suspension-manager/main.go

docker-build:
	docker build --target state-informer -t sandbox-state-informer:latest -f Dockerfile ../..
	docker build --target manager -t sandbox-suspension-manager:latest -f Dockerfile ../..
	docker build --target router -t sandbox-router:latest -f Dockerfile ../..

kind-load: docker-build
	kind load docker-image sandbox-state-informer:latest --name $(KIND_CLUSTER)
	kind load docker-image sandbox-suspension-manager:latest --name $(KIND_CLUSTER)
	kind load docker-image sandbox-router:latest --name $(KIND_CLUSTER)

deploy: deploy-envoy

deploy-envoy: kind-load
	kubectl apply -f manifests/common/
	kubectl apply -f manifests/envoy-gateway/

test:
	@echo "--- Testing Tenant-A Sandboxes ---"
	curl -v -H "Host: sandbox-a1.tenant-a.sandbox.local" http://localhost:8080/
	curl -v -H "Host: sandbox-a2.tenant-a.sandbox.local" http://localhost:8080/
	@echo "--- Testing Tenant-B Sandboxes ---"
	curl -v -H "Host: sandbox-b1.tenant-b.sandbox.local" http://localhost:8080/

clean:
	kubectl delete -f manifests/common/ --ignore-not-found=true
	kubectl delete -f manifests/envoy-gateway/ --ignore-not-found=true
