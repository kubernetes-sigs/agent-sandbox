# Makefile for Envoy-Based Auto-Resume Ingress Interception PoC

.PHONY: build docker-build kind-load deploy deploy-envoy test clean

IMAGE_NAME ?= sandbox-router:latest
KIND_CLUSTER ?= agent-sandbox

build:
	go build -o ../../bin/sandbox-router ./cmd/sandbox-router/main.go
	go build -o ../../bin/sandbox-suspension-manager ./cmd/suspension-manager/main.go

docker-build:
	docker build --target state-informer -t sandbox-router:latest -f Dockerfile ../..
	docker build --target manager -t sandbox-suspension-manager:latest -f Dockerfile ../..

kind-load: docker-build
	kind load docker-image sandbox-router:latest --name $(KIND_CLUSTER)
	kind load docker-image sandbox-suspension-manager:latest --name $(KIND_CLUSTER)

deploy: deploy-envoy-epp

deploy-envoy-epp: kind-load
	@echo "--- Installing Gateway API CRDs ---"
	kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/experimental-install.yaml --server-side || true
	@echo "--- Installing Envoy Gateway (v1.8.0) ---"
	kubectl create namespace envoy-gateway-system --dry-run=client -o yaml | kubectl apply -f -
	helm template eg oci://docker.io/envoyproxy/gateway-helm --version v1.8.0 -n envoy-gateway-system --include-crds | kubectl apply --server-side -f -
	@echo "--- Installing Gateway API Inference Extension (GAIE) ---"
	kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.0.2/manifests.yaml --server-side
	@echo "--- Installing Envoy AI Gateway (v1.0.0) ---"
	kubectl create namespace envoy-ai-gateway-system --dry-run=client -o yaml | kubectl apply -f -
	helm template envoy-ai-gateway-crds oci://docker.io/envoyproxy/ai-gateway-crds-helm --version v1.0.0 -n envoy-ai-gateway-system | kubectl apply --server-side -f -
	helm template envoy-ai-gateway oci://docker.io/envoyproxy/ai-gateway-helm --version v1.0.0 -n envoy-ai-gateway-system | kubectl apply --server-side -f -
	@echo "--- Applying Manifests (HTTPRoute + InferencePool) ---"
	kubectl apply -f manifests/common/
	kubectl apply -f manifests/envoy-gateway-epp/
	kubectl rollout restart deployment/sandbox-router -n tenant-a || true
	kubectl rollout restart deployment/sandbox-router -n tenant-b || true
	@echo "--- Restarting Envoy Gateway to pick up extension config ---"
	kubectl rollout restart -n envoy-gateway-system deployment/envoy-gateway
	kubectl rollout status -n envoy-gateway-system deployment/envoy-gateway --timeout=60s

test:
	@echo "--- Testing Tenant-A Sandboxes via HTTPRoute ---"
	curl -i -H "Host: sandbox-a1.tenant-a.sandbox.local" http://localhost:8080/
	curl -i -H "Host: sandbox-a2.tenant-a.sandbox.local" http://localhost:8080/
	@echo "--- Testing Tenant-B Sandboxes via HTTPRoute ---"
	curl -i -H "Host: sandbox-b1.tenant-b.sandbox.local" http://localhost:8080/

clean:
	kubectl delete -f manifests/common/ --ignore-not-found=true
	kubectl delete -f manifests/envoy-gateway-epp/ --ignore-not-found=true
