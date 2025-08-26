.PHONY: all
all: generate build

.PHONY: generate
generate: install-generate-tools
	go generate

.PHONY: install-generate-tools
install-generate-tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	go install github.com/B1NARY-GR0UP/nwa@latest

.PHONY: build
build:
	go build -o bin/manager cmd/main.go
